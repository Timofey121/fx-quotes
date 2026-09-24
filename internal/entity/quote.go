package entity

import "time"

type Quote struct {
	Pair       Pair
	Rate       Rate
	Provider   string
	SourceDate time.Time
	UpdatedAt  time.Time
}
