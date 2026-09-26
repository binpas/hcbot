// Command markovtrain trains the bot's Markov chat from files. Each file is
// plain text with one message on each line, or a DiscordChatExporter .json
// export. It uses the blocklist from the main database, which it only
// reads. It can run while the bot runs.
//
//	markovtrain [-db markov.db] [-maindb health_bot.db] file...
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"

	"github.com/binpas/hcbot/internal/db"
	"github.com/binpas/hcbot/internal/markov"
)

const step = 5000 // messages for each transaction

func main() {
	log.SetFlags(0)
	chainPath := flag.String("db", "markov.db", "the Markov database to train")
	mainPath := flag.String("maindb", "health_bot.db", "the bot's main database, for the blocklist")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags] file...\n\n"+
			"Each file is plain text (one message per line) or a DiscordChatExporter .json export.\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	entries, err := db.ReadMarkovBlocklist(*mainPath)
	if _, statErr := os.Stat(*mainPath); errors.Is(statErr, fs.ErrNotExist) {
		log.Printf("warning: %s not found; training with no blocklist", *mainPath)
	} else if err != nil {
		log.Fatalf("read blocklist from %s: %v", *mainPath, err)
	}
	bl := markov.NewBlocklist(entries)
	log.Printf("blocklist: %d word(s) or phrase(s)", bl.Len())

	chain, err := markov.Open(*chainPath)
	if err != nil {
		log.Fatalf("open %s: %v", *chainPath, err)
	}
	defer func() { _ = chain.Close() }()

	var learned, blocked, empty int
	for _, path := range flag.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Fatal(err)
		}
		lines, err := markov.ParseImport(path, data)
		if err != nil {
			log.Fatalf("%s: %v", path, err)
		}
		kept, b := markov.Prepare(lines, bl)
		for i := 0; i < len(kept); i += step {
			end := min(i+step, len(kept))
			if err := chain.Learn(kept[i:end]); err != nil {
				log.Fatalf("%s: learn: %v", path, err)
			}
			fmt.Fprintf(os.Stderr, "\r%s: %d/%d messages", path, end, len(kept))
		}
		fmt.Fprintln(os.Stderr)
		learned += len(kept)
		blocked += b
		empty += len(lines) - len(kept) - b
	}

	st, err := chain.Stats()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("learned %d message(s); skipped %d blocked and %d empty", learned, blocked, empty)
	log.Printf("%s: %d words, %d word steps, %.1f MB", *chainPath, st.Words, st.Transitions, float64(st.Bytes)/(1<<20))
}
