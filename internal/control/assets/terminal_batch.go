package assets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tianwei/diskless/internal/control/errs"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
	"github.com/tianwei/diskless/internal/xlsx"
)

var ErrTerminalImportInvalid = errs.Invalid("终端导入文件无效")

type TerminalImportError struct {
	Row   int    `json:"row"`
	Error string `json:"error"`
}

type TerminalImportResult struct {
	Created int                   `json:"created"`
	Errors  []TerminalImportError `json:"errors"`
	Items   []domain.Terminal     `json:"items"`
}

var terminalImportHeader = []string{"MAC", "IP", "Name", "GroupID", "IsSuper", "State"}

type terminalImportColumns struct {
	macIdx     int
	ipIdx      int
	nameIdx    int
	groupIDIdx int
	isSuperIdx int
	stateIdx   int
}

func resolveTerminalImportColumns(header []string) terminalImportColumns {
	cols := terminalImportColumns{
		macIdx:     0,
		ipIdx:      1,
		groupIDIdx: 2,
		isSuperIdx: 3,
		stateIdx:   4,
		nameIdx:    -1,
	}
	for index, value := range header {
		switch strings.TrimSpace(strings.ToLower(value)) {
		case "mac":
			cols.macIdx = index
		case "ip":
			cols.ipIdx = index
		case "name":
			cols.nameIdx = index
		case "groupid", "group_id":
			cols.groupIDIdx = index
		case "issuper", "is_super":
			cols.isSuperIdx = index
		case "state":
			cols.stateIdx = index
		}
	}
	return cols
}

func TerminalTemplateXLSX() ([]byte, error) {
	return xlsx.Write([][]string{terminalImportHeader})
}

func (s TerminalService) Export(ctx context.Context, groupID string) ([]byte, error) {
	result, err := s.List(ctx, groupID)
	if err != nil {
		return nil, err
	}
	rows := [][]string{terminalImportHeader}
	for _, terminal := range result.Items {
		rows = append(rows, []string{
			terminal.MAC,
			terminal.IP,
			terminal.Name,
			terminal.GroupID,
			formatBool(terminal.IsSuper),
			string(terminal.State),
		})
	}
	return xlsx.Write(rows)
}

func (s TerminalService) Import(ctx context.Context, data []byte) (TerminalImportResult, error) {
	rows, err := xlsx.Read(data)
	if err != nil || len(rows) == 0 {
		return TerminalImportResult{}, ErrTerminalImportInvalid
	}
	columns := resolveTerminalImportColumns(rows[0])

	terminalAllocMu.Lock()
	defer terminalAllocMu.Unlock()

	groups, terminals, err := loadTerminalImportState(ctx, s.Store)
	if err != nil {
		return TerminalImportResult{}, err
	}
	occupiedIPs := map[uint32]bool{}
	usedMACs := map[string]bool{}
	groupCounts := map[string]int{}
	disks, err := s.Store.GroupDisks().List(ctx)
	if err != nil {
		return TerminalImportResult{}, err
	}
	// 超管机占用哪些配置（系统盘和数据盘）与 EnsureSuperAvailable 同口径，这里对整份文件一次算完。
	superConfigs := map[string]domain.Terminal{}
	holdConfigs := func(terminal domain.Terminal) {
		if group, ok := groups[terminal.GroupID]; ok {
			for configID := range groupConfigIDs(group, disks) {
				superConfigs[configID] = terminal
			}
		}
	}
	for _, terminal := range terminals {
		ip, err := parseTerminalIP(terminal.IP)
		if err != nil {
			return TerminalImportResult{}, err
		}
		occupiedIPs[ipv4Uint32(ip)] = true
		usedMACs[terminal.MAC] = true
		groupCounts[terminal.GroupID]++
		if terminal.IsSuper {
			holdConfigs(terminal)
		}
	}

	// 文件里显式写下的地址不能被留空的行抢走，即使那行在后面；
	// 否则按行序分配会让后面那行报「终端 IP 已存在」，整份导入被拒且原因无从看出。
	avoid := map[uint32]bool{}
	for ip := range occupiedIPs {
		avoid[ip] = true
	}
	for _, row := range rows[1:] {
		if emptyRow(row) {
			continue
		}
		if ip, err := parseTerminalIP(cell(row, columns.ipIdx)); err == nil {
			avoid[ipv4Uint32(ip)] = true
		}
	}
	reserveClusterAddrs(ctx, s.Store, avoid)

	var result TerminalImportResult
	for i, row := range rows[1:] {
		rowNum := i + 2
		if emptyRow(row) {
			continue
		}
		terminal, err := s.parseTerminalImportRow(row, columns, groups, disks, occupiedIPs, avoid, usedMACs, groupCounts, superConfigs)
		if err == nil && terminal.IsSuper {
			err = refuseSuperDuringConfigChange(ctx, s.Store, groupConfigIDs(groups[terminal.GroupID], disks))
		}
		if err != nil {
			result.Errors = append(result.Errors, TerminalImportError{Row: rowNum, Error: err.Error()})
			continue
		}
		result.Items = append(result.Items, terminal)
		ip, _ := parseTerminalIP(terminal.IP)
		occupiedIPs[ipv4Uint32(ip)] = true
		avoid[ipv4Uint32(ip)] = true
		usedMACs[terminal.MAC] = true
		groupCounts[terminal.GroupID]++
		if terminal.IsSuper {
			holdConfigs(terminal)
		}
	}
	if len(result.Errors) > 0 {
		return result, nil
	}
	if err := s.Store.Tx(ctx, func(tx store.Store) error {
		for _, terminal := range result.Items {
			if err := tx.Terminals().Create(ctx, terminal); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return TerminalImportResult{}, err
	}
	if err := syncDHCP(ctx, s.Store, s.DHCP, s.Logger); err != nil {
		return TerminalImportResult{}, err
	}
	result.Created = len(result.Items)
	return result, nil
}

func (s TerminalService) parseTerminalImportRow(row []string, columns terminalImportColumns, groups map[string]domain.Group, disks []domain.GroupDisk, occupiedIPs, avoid map[uint32]bool, usedMACs map[string]bool, groupCounts map[string]int, superConfigs map[string]domain.Terminal) (domain.Terminal, error) {
	mac, err := normalizeTerminalMAC(cell(row, columns.macIdx))
	if err != nil {
		return domain.Terminal{}, err
	}
	if usedMACs[mac] {
		return domain.Terminal{}, ErrTerminalMACExists
	}
	groupID := strings.TrimSpace(cell(row, columns.groupIDIdx))
	group, ok := groups[groupID]
	if !ok {
		return domain.Terminal{}, fmt.Errorf("group not found")
	}
	state, err := normalizeTerminalState(domain.TerminalState(strings.TrimSpace(cell(row, columns.stateIdx))), "")
	if err != nil {
		return domain.Terminal{}, err
	}
	isSuper, err := parseBool(cell(row, columns.isSuperIdx))
	if err != nil {
		return domain.Terminal{}, err
	}
	if isSuper {
		for configID := range groupConfigIDs(group, disks) {
			if occupied, ok := superConfigs[configID]; ok && occupied.MAC != mac {
				return domain.Terminal{}, TerminalSuperConflict{ConfigID: configID, TerminalID: occupied.ID, MAC: occupied.MAC}
			}
		}
	}
	if groupCounts[groupID] >= group.ClientMax {
		return domain.Terminal{}, ErrTerminalGroupFull
	}
	ipText := strings.TrimSpace(cell(row, columns.ipIdx))
	if ipText == "" {
		// avoid 比 occupiedIPs 多出文件里显式钉死的那些地址。
		ipText, err = allocateIPFromGroup(group, avoid)
		if err != nil {
			return domain.Terminal{}, err
		}
	} else {
		ip, err := parseTerminalIP(ipText)
		if err != nil {
			return domain.Terminal{}, err
		}
		if occupiedIPs[ipv4Uint32(ip)] {
			return domain.Terminal{}, ErrTerminalIPExists
		}
		if err := ensureTerminalIPInGroup(ipText, group); err != nil {
			return domain.Terminal{}, err
		}
	}
	name := strings.TrimSpace(cell(row, columns.nameIdx))
	return domain.Terminal{
		ID:      "terminal-" + mac,
		Name:    name,
		MAC:     mac,
		IP:      ipText,
		GroupID: groupID,
		IsSuper: isSuper,
		State:   state,
	}, nil
}

func loadTerminalImportState(ctx context.Context, st store.Store) (map[string]domain.Group, []domain.Terminal, error) {
	groups, err := st.Groups().List(ctx)
	if err != nil {
		return nil, nil, err
	}
	terminals, err := st.Terminals().List(ctx)
	if err != nil {
		return nil, nil, err
	}
	groupMap := map[string]domain.Group{}
	for _, group := range groups {
		groupMap[group.ID] = group
	}
	return groupMap, terminals, nil
}

func allocateIPFromGroup(group domain.Group, occupied map[uint32]bool) (string, error) {
	pool, err := groupAddressPool(group.StartIP, group.ClientMax)
	if err != nil {
		return "", err
	}
	ip, ok := pool.allocate(occupied)
	if !ok {
		return "", groupFullError{group: group.Name, from: ipv4String(pool.start), to: ipv4String(pool.end)}
	}
	return ip, nil
}

// groupFullError 点名哪个分组、哪段地址已用完以及怎么办；仍算 ErrTerminalNoAvailableIP。
type groupFullError struct{ group, from, to string }

func (e groupFullError) Error() string {
	return fmt.Sprintf("分组「%s」没有空闲地址（%s–%s 已全部分配）。请在「分组管理」中调大该组的最大客户机数，或改选其他分组", e.group, e.from, e.to)
}

func (e groupFullError) Is(target error) bool { return errors.Is(ErrTerminalNoAvailableIP, target) }

func parseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "false", "0", "no", "n", "否":
		return false, nil
	case "true", "1", "yes", "y", "是":
		return true, nil
	default:
		return false, fmt.Errorf("invalid boolean")
	}
}

func formatBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func emptyRow(row []string) bool {
	for _, value := range row {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}

func cell(row []string, index int) string {
	if index < 0 || index >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[index])
}
