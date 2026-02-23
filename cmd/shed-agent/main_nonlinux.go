//go:build !linux
// +build !linux

package main

import "log"

func main() {
	log.Fatalf("shed-agent is only supported on linux")
}
