// Recall — локальный RAG-поиск по своим документам.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/WOLFraMix/recall/internal/api"
	"github.com/WOLFraMix/recall/internal/config"
	"github.com/WOLFraMix/recall/internal/ingest"
	"github.com/WOLFraMix/recall/internal/llm"
	"github.com/WOLFraMix/recall/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("recall: ")

	cfg := config.Load()

	st, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	provider := llm.NewOllama(cfg.LLMURL, cfg.EmbedModel, cfg.ChatModel, cfg.HTTPTimeout)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ing := ingest.New(st, provider, cfg.EmbedModel, cfg.IngestWorkers, cfg.QueueSize)
	go ing.Run(ctx)

	srv := api.New(cfg, st, ing, provider)
	go func() {
		log.Printf("running on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
			log.Fatalf("api: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down...")

	stop()
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Printf("api shutdown: %v", err)
	}
	ing.Wait()
	log.Print("bye")
}
