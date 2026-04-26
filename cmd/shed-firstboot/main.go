//go:build linux

package main

import (
	"log"
	"os"
)

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("shed-firstboot: ")
	if err := runFirstboot(defaultCfg()); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}
