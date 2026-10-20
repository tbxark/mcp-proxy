//go:build race

package main

// raceEnabled lets timing tests skip under the race detector, which slows
// the code they measure by an order of magnitude.
const raceEnabled = true
