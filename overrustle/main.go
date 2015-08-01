package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"runtime"
	"time"
)

// immutable config
const (
	SocketHandshakeTimeout = 10 * time.Second
	SocketReconnectDelay   = 20 * time.Second
	MessageBufferSize      = 100
)

func init() {
	configPath := flag.String("config", "", "config path")
	flag.Parse()
	SetupConfig(*configPath)
}

func main() {
	// runtime.GOMAXPROCS(runtime.NumCPU())
	runtime.GOMAXPROCS(4)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	logs := NewChatLogs()

	dc := NewDestinyChat()
	dl := NewDestinyLogger(logs)
	go dl.Log(dc.Messages())
	go dc.Run()

	tc := NewTwitchChat(func(ch string, m chan *Message) {
		log.Printf("started logging %s", ch)
		tl := NewTwitchLogger(logs, ch)
		go func() {
			tl.Log(m)
			log.Printf("stopped logging %s", ch)
		}()
	})
	go tc.Run()

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	select {
	case <-sigint:
		log.Println("i love you guys, be careful")
	}
}
