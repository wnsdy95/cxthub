package domain

type GraphReadiness struct {
	Selected   bool                  `json:"selected"`
	Projection GraphProjectionStatus `json:"projection"`
}
