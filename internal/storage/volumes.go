package storage

// 节点报事实，写入者归类：备机的库不权威，这里的结构只装 ZFS 和文件系统读到的东西。

// PoolSnapshotFact 是一个快照。名字是 @ 之后的部分，Clones 是依赖它的数据集（池内相对名）。
type PoolSnapshotFact struct {
	Name    string   `json:"name"`
	GUID    string   `json:"guid"`
	Used    int64    `json:"used"`
	Created int64    `json:"created"`
	Clones  []string `json:"clones,omitempty"`
}

// PoolDatasetFact 是池里一个文件系统或卷。Name、Origin 都是池内相对名，池根的 Name 为空。
// 跨节点比对只能用快照的 GUID：send/recv 不保留文件系统的 GUID，数据集的 Created 也是本机收下的时间。
type PoolDatasetFact struct {
	Name            string             `json:"name"`
	Type            string             `json:"type"`
	GUID            string             `json:"guid"`
	Used            int64              `json:"used"`
	Referenced      int64              `json:"referenced"`
	UsedBySnapshots int64              `json:"used_by_snapshots"`
	VolSize         int64              `json:"volsize"`
	Origin          string             `json:"origin,omitempty"`
	Created         int64              `json:"created"`
	Snapshots       []PoolSnapshotFact `json:"snapshots,omitempty"`
}

// PoolFileFact 是文件库里的一个文件或目录。Size 是文件长度，Used 是实际占用（压缩后）。
type PoolFileFact struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Used     int64  `json:"used"`
	Modified int64  `json:"modified"`
}

// PoolFilesFact 是 nd/files 里的内容。Readable 为 false 表示没挂载或读不了，不代表没有文件。
type PoolFilesFact struct {
	Readable bool           `json:"readable"`
	Imports  []PoolFileFact `json:"imports,omitempty"`
	Dirs     []PoolFileFact `json:"dirs,omitempty"`
}

// PoolFacts 是一个节点上一个池的全部内容。
type PoolFacts struct {
	Node      string            `json:"node"`
	Pool      string            `json:"pool"`
	DataPool  string            `json:"data_pool"`
	Used      int64             `json:"used"`
	Available int64             `json:"available"`
	Freeing   int64             `json:"freeing"`
	Datasets  []PoolDatasetFact `json:"datasets"`
	Files     *PoolFilesFact    `json:"files,omitempty"`
	// PullError 是本节点最近一次拉取复制的错误，只有备机上才可能有。
	PullError string `json:"pull_error,omitempty"`
}
