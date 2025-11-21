// main executable.
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/bluenviron/mediamtx/internal/core"
	"github.com/joho/godotenv"
)

func main() {
	// Load .env file from parent directory
	exePath, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exePath)
		envPath := filepath.Join(exeDir, ".env")
		
		if err := godotenv.Load(envPath); err != nil {
			// Fallback: try current directory
			if err := godotenv.Load(".env"); err != nil {
				log.Printf("Warning: .env file not found, using system environment variables")
			}
		}
	}
	s, ok := core.New(os.Args[1:])
	if !ok {
		os.Exit(1)
	}
	s.Wait()
}
