package main

import (
	"cmp"
	"slices"
	"strconv"
)

func num(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// Tracking is chronyd's current clock and reference status.
type Tracking struct {
	RefID, Name, Stratum, Leap string
	RefTime, SysTime           float64
	LastOffset, RMSOffset      float64
	Freq, Skew                 float64
	RootDelay, RootDispersion  float64
}

// Source is one of chronyd's time sources.
type Source struct {
	Mode, State, Name, Stratum, Reach string
	Poll                              int
	LastRx, Offset, Err               float64
}

// Client describes the NTP traffic from one client.
type Client struct {
	Name             string
	Packets, Dropped float64
	// Interval is log2 seconds between packets; unknownInterval if not known.
	Interval int
}

const unknownInterval = 127

// maxOffenders is more rows than any terminal shows.
const maxOffenders = 200

// topOffenders sorts clients by most drops, then most frequent, and keeps the first maxOffenders.
func topOffenders(clients []Client) []Client {
	slices.SortFunc(clients, func(a, b Client) int {
		return cmp.Or(cmp.Compare(b.Dropped, a.Dropped), cmp.Compare(a.Interval, b.Interval), cmp.Compare(a.Name, b.Name))
	})
	return slices.Clone(clients[:min(len(clients), maxOffenders)])
}
