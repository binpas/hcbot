// Command gohealthy runs the HEALTH Discord bot.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/binpas/hcbot/internal/bot"
	"github.com/binpas/hcbot/internal/config"
	"github.com/binpas/hcbot/internal/db"
)

const dbPath = "health_bot.db"

func main() {
	// Under systemd the journal adds its own timestamps.
	if os.Getenv("JOURNAL_STREAM") != "" {
		log.SetFlags(0)
	}
	env := config.LoadEnv()
	if env.BotToken == "" {
		log.Fatal("BOT_TOKEN is not set")
	}

	database, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			log.Printf("close database: %v", err)
		}
	}()

	b, err := bot.New(env, database)
	if err != nil {
		log.Fatalf("create bot: %v", err)
	}
	if err := b.Open(); err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := b.Close(); err != nil {
			log.Printf("close session: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Print("shutting down")
}
