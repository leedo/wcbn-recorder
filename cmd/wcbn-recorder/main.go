// Command wcbn-recorder watches the WCBN schedule and records selected shows
// from the live stream, without transcoding. Shows are selected in a web UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"wcbn-recorder/internal/config"
	"wcbn-recorder/internal/recorder"
	"wcbn-recorder/internal/spinitron"
	"wcbn-recorder/internal/store"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	list := flag.Bool("list", false, "print upcoming selected shows and exit")
	test := flag.Duration("test", 0, "record a short test clip of this length now and exit (e.g. 30s)")
	shows := flag.String("shows", "", "write all shows on the schedule, with descriptions, to this text file and exit")
	flag.Parse()
	if os.Getenv("JOURNAL_STREAM") != "" {
		log.SetFlags(0) // journald adds its own timestamps
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case *test > 0:
		now := time.Now()
		job := recorder.Job{
			Program: "Test",
			Event:   spinitron.Event{Title: "Test Recording", Start: now, End: now.Add(*test)},
			Until:   now.Add(*test),
		}
		if err := recorder.Record(ctx, cfg, nil, job); err != nil {
			log.Fatal(err)
		}
		return
	case *shows != "":
		if err := recorder.WriteShowList(ctx, cfg, *shows); err != nil {
			log.Fatal(err)
		}
		return
	}

	db, err := store.Open(cfg.Database)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.ImportPrograms(ctx, cfg.Programs); err != nil {
		log.Fatal(err)
	}
	app := recorder.NewApp(cfg, db)

	if *list {
		if !app.Refresh(ctx) {
			os.Exit(1)
		}
		jobs, err := app.Upcoming(ctx, time.Now())
		if err != nil {
			log.Fatal(err)
		}
		for _, j := range jobs {
			fmt.Printf("%-30s  %s – %s\n", j.Event.Title,
				j.Event.Start.In(cfg.Location).Format("Mon Jan 2 3:04 PM"),
				j.Event.End.In(cfg.Location).Format("3:04 PM"))
		}
		return
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: app.Handler()}
	go func() {
		log.Printf("web UI at http://%s/", cfg.Listen)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("web UI: %v", err)
		}
	}()
	app.Run(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}
