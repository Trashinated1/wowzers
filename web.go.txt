package main

import (
	"embed"
	"io/fs"
	"log"
)

//go:embed web/*
var webFS embed.FS

// webRoot strips the "web/" prefix so that files are served at the root
var webRoot fs.FS

func init() {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("failed to create sub filesystem for web assets: %v", err)
	}
	webRoot = sub
}
