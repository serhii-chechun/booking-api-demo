package main

import (
	"fmt"
	"os"

	"booking-api-demo/internal/app/server"
)

func main() {
	if err := server.New().Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
