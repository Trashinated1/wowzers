package main

import (
    "flag"
    "log"
    "os"
)

func main() {
    addr := flag.String("addr", ":7654", "HTTP listen address")
    flag.Parse()

    arl := os.Getenv("DEEZER_ARL")
    if arl == "" {
        log.Fatal("DEEZER_ARL environment variable is not set. Please provide your Deezer ARL.")
    }

    // Read quality from env, default to high (320kbps)
    quality := os.Getenv("DEEZER_QUALITY")
    if quality == "" {
        quality = "high"
    }

    srv, err := NewServer(*addr, arl, quality)
    if err != nil {
        log.Fatalf("init: %v", err)
    }
    log.Printf("Wowzers listening on http://localhost%s", *addr)
    log.Printf("Music quality set to: %s", quality)
    log.Fatal(srv.Run())
}