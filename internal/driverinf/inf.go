// Package driverinf 解析 Windows 驱动 INF 文件，提取驱动中心需要的元数据：厂商、设备类、版本、
// 发布日期以及驱动声称支持的硬件 ID。
//
// 故意宽容：现实中的 INF 混用 UTF-16、UTF-8 和 GBK 编码，引用 [Strings] 标记，
// 并按目标平台给型号节加修饰（如 [Intel.NTamd64.10.0]）。
package driverinf

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

type Driver struct {
	Provider    string
	ClassName   string
	Version     string
	ReleaseDate string
	CatalogFile string
	HWIDs       []string
}

type entry struct {
	key    string
	values []string
}

// Parse 从单个 INF 文件中提取驱动元数据。
func Parse(data []byte) (Driver, error) {
	text, err := decode(data)
	if err != nil {
		return Driver{}, err
	}
	sections := parseSections(text)
	if len(sections) == 0 {
		return Driver{}, fmt.Errorf("not a valid inf file: no sections found")
	}
	strTable := stringsTable(sections)
	resolve := func(v string) string { return replaceTokens(v, strTable) }

	var d Driver
	for _, e := range sections["version"] {
		switch strings.ToLower(e.key) {
		case "provider":
			d.Provider = resolve(first(e.values))
		case "class":
			d.ClassName = resolve(first(e.values))
		case "driverver":
			if len(e.values) > 0 {
				d.ReleaseDate = strings.TrimSpace(e.values[0])
			}
			if len(e.values) > 1 {
				d.Version = strings.TrimSpace(e.values[1])
			}
		default:
			if strings.HasPrefix(strings.ToLower(e.key), "catalogfile") && d.CatalogFile == "" {
				d.CatalogFile = resolve(first(e.values))
			}
		}
	}

	d.HWIDs = collectHWIDs(sections, resolve)
	return d, nil
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// collectHWIDs 遍历 [Manufacturer] 找到型号节（基础名及所有平台修饰），收集其中列出的硬件 ID。
func collectHWIDs(sections map[string][]entry, resolve func(string) string) []string {
	modelSections := map[string]bool{}
	for _, e := range sections["manufacturer"] {
		if len(e.values) == 0 {
			continue
		}
		base := strings.ToLower(strings.TrimSpace(resolve(e.values[0])))
		if base == "" {
			continue
		}
		modelSections[base] = true
		for _, deco := range e.values[1:] {
			deco = strings.ToLower(strings.TrimSpace(deco))
			if deco != "" {
				modelSections[base+"."+deco] = true
			}
		}
	}

	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		id := strings.ToUpper(strings.TrimSpace(resolve(raw)))
		if !looksLikeHWID(id) || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	// 直接遍历 map 顺序是随机的，而这个列表会落库并展示给界面，同一文件解析两次必须一致。
	// 节名排序，节内条目保持 INF 声明的顺序。
	matched := make([]string, 0, len(sections))
	for name := range sections {
		if matchesModelSection(name, modelSections) {
			matched = append(matched, name)
		}
	}
	sort.Strings(matched)
	for _, name := range matched {
		for _, e := range sections[name] {
			// device-description = install-section, hwid [, compatible-ids...]
			for _, v := range e.values[min(1, len(e.values)):] {
				add(v)
			}
		}
	}
	return out
}

// matchesModelSection 判断节是否属于 [Manufacturer] 声明的型号节，包括厂商行没明确列出的修饰变体（有些 INF 会这样）。
func matchesModelSection(name string, models map[string]bool) bool {
	if models[name] {
		return true
	}
	for base := range models {
		if strings.HasPrefix(name, base+".nt") {
			return true
		}
	}
	return false
}

func looksLikeHWID(id string) bool {
	if id == "" {
		return false
	}
	return strings.Contains(id, `\`) || strings.HasPrefix(id, "*")
}

func stringsTable(sections map[string][]entry) map[string]string {
	table := map[string]string{}
	for name, entries := range sections {
		if name != "strings" && !strings.HasPrefix(name, "strings.") {
			continue
		}
		for _, e := range entries {
			if _, ok := table[strings.ToLower(e.key)]; !ok {
				table[strings.ToLower(e.key)] = first(e.values)
			}
		}
	}
	return table
}

func replaceTokens(v string, table map[string]string) string {
	if !strings.Contains(v, "%") {
		return v
	}
	var b strings.Builder
	rest := v
	for {
		start := strings.IndexByte(rest, '%')
		if start < 0 {
			b.WriteString(rest)
			break
		}
		end := strings.IndexByte(rest[start+1:], '%')
		if end < 0 {
			b.WriteString(rest)
			break
		}
		token := rest[start+1 : start+1+end]
		b.WriteString(rest[:start])
		if replacement, ok := table[strings.ToLower(token)]; ok {
			b.WriteString(replacement)
		} else {
			b.WriteString("%" + token + "%")
		}
		rest = rest[start+end+2:]
	}
	return strings.TrimSpace(b.String())
}

func parseSections(text string) map[string][]entry {
	sections := map[string][]entry{}
	current := ""
	for _, line := range splitLogicalLines(text) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				continue
			}
			current = strings.ToLower(strings.TrimSpace(line[1:end]))
			if _, ok := sections[current]; !ok {
				sections[current] = nil
			}
			continue
		}
		if current == "" {
			continue
		}
		key, values := parseLine(line)
		if key == "" && len(values) == 0 {
			continue
		}
		sections[current] = append(sections[current], entry{key: key, values: values})
	}
	return sections
}

// splitLogicalLines 按换行拆分，去掉注释，并拼接以反斜杠结尾的续行。
func splitLogicalLines(text string) []string {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string
	pending := ""
	for _, line := range raw {
		line = stripComment(line)
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, `\`) && !strings.HasSuffix(trimmed, `\\`) {
			pending += strings.TrimSuffix(trimmed, `\`)
			continue
		}
		out = append(out, pending+line)
		pending = ""
	}
	if pending != "" {
		out = append(out, pending)
	}
	return out
}

func stripComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case ';':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}

func parseLine(line string) (string, []string) {
	key := ""
	rest := line
	if eq := indexOutsideQuotes(line, '='); eq >= 0 {
		key = strings.TrimSpace(unquote(line[:eq]))
		rest = line[eq+1:]
	}
	var values []string
	for _, part := range splitOutsideQuotes(rest, ',') {
		values = append(values, strings.TrimSpace(unquote(strings.TrimSpace(part))))
	}
	// 去掉 "key =" 这种行产生的末尾单个空值。
	for len(values) > 0 && values[len(values)-1] == "" {
		values = values[:len(values)-1]
	}
	return key, values
}

func indexOutsideQuotes(s string, ch byte) int {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case ch:
			if !inQuote {
				return i
			}
		}
	}
	return -1
}

func splitOutsideQuotes(s string, sep byte) []string {
	var out []string
	start := 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case sep:
			if !inQuote {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// decode 把 INF 原始字节转成字符串，处理 UTF-16（BOM）、UTF-8 和 GBK（中文区驱动包常见的旧编码）。
func decode(data []byte) (string, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		return decodeUTF16(data[2:], false), nil
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return decodeUTF16(data[2:], true), nil
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		return string(data[3:]), nil
	}
	// 偶尔会遇到无 BOM 的 UTF-16LE：按 NUL 密度检测。
	if looksLikeUTF16LE(data) {
		return decodeUTF16(data, false), nil
	}
	if utf8.Valid(data) {
		return string(data), nil
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(data)
	if err != nil {
		return "", fmt.Errorf("undecodable inf encoding: %w", err)
	}
	return string(decoded), nil
}

func looksLikeUTF16LE(data []byte) bool {
	if len(data) < 4 || len(data)%2 != 0 {
		return false
	}
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}
	nulOdd := 0
	for i := 1; i < len(sample); i += 2 {
		if sample[i] == 0 {
			nulOdd++
		}
	}
	return nulOdd > len(sample)/4
}

func decodeUTF16(data []byte, bigEndian bool) string {
	if len(data)%2 != 0 {
		data = data[:len(data)-1]
	}
	u16 := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		if bigEndian {
			u16 = append(u16, uint16(data[i])<<8|uint16(data[i+1]))
		} else {
			u16 = append(u16, uint16(data[i+1])<<8|uint16(data[i]))
		}
	}
	return string(utf16.Decode(u16))
}
