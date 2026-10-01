package main

import (
	"context"
	runner "github.com/laudryfadian/griyo-backend-service-runner/internal"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	endpoint := os.Getenv("GRIYO_URL")
	token := os.Getenv("RUNNER_TOKEN")
	if endpoint == "" || token == "" {
		log.Fatal("GRIYO_URL and RUNNER_TOKEN required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if e := runner.New(endpoint, token).Run(ctx); e != nil {
		log.Fatal(e)
	}
}
