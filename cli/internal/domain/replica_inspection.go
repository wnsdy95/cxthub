package domain

type ReplicaInspection struct {
	Completed        bool     `json:"completed"`
	Snapshots        int      `json:"snapshots"`
	DocumentsChecked int      `json:"documents_checked"`
	HistoryEvents    int      `json:"history_events"`
	Issues           []string `json:"issues"`
}
