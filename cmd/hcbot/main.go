// Command hcbot runs the HEALTH Discord bot.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/binpas/hcbot/internal/bot"
	"github.com/binpas/hcbot/internal/config"
	"github.com/binpas/hcbot/internal/db"
	"github.com/binpas/hcbot/internal/markov"
)

const (
	dbPath    = "hcbot.db"
	oldDBPath = "health_bot.db" // the name before hcbot.db
)

// version is the git commit, set by the Makefile (-X main.version=...).
var version = "dev"

func main() {
	// Under systemd the journal adds its own timestamps.
	if os.Getenv("JOURNAL_STREAM") != "" {
		log.SetFlags(0)
	}
	env := config.LoadEnv()
	if env.BotToken == "" {
		log.Fatal("BOT_TOKEN is not set")
	}

	if renamed, err := db.RenameFile(oldDBPath, dbPath); err != nil {
		log.Fatalf("rename %s to %s: %v", oldDBPath, dbPath, err)
	} else if renamed {
		log.Printf("Renamed %s to %s.", oldDBPath, dbPath)
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

	chain, err := markov.Open(env.MarkovDBPath)
	if err != nil {
		log.Fatalf("open markov database: %v", err)
	}
	defer func() {
		if err := chain.Close(); err != nil {
			log.Printf("close markov database: %v", err)
		}
	}()

	b, err := bot.New(env, database, chain)
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
