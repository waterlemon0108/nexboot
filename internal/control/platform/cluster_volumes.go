package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/tianwei/diskless/internal/storage"
)

// FetchNodeVolumes 读别的节点上一个池的内容。节点的报错原样带回，写入者据此说明读不到的原因。
func FetchNodeVolumes(ctx context.Context, apiURL, token, pool string) (storage.PoolFacts, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/internal/node/volumes?pool="+url.QueryEscape(pool), nil)
	if err != nil {
		return storage.PoolFacts{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return storage.PoolFacts{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return storage.PoolFacts{}, fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}
	var facts storage.PoolFacts
	if err := json.NewDecoder(resp.Body).Decode(&facts); err != nil {
		return storage.PoolFacts{}, err
	}
	return facts, nil
}
