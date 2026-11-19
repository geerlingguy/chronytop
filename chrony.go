package main

import "strconv"

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
