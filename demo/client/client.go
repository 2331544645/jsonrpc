package main

import (
	"flag"
	"github.com/gorilla/websocket"
	"jsonrpc"
	"jsonrpc/demo/data"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var addr = flag.String("addr", "localhost:8080", "http service address")

type Peer struct {
}

func (c *Peer) SayHello(msg data.Incoming) (*data.Outputting, error) {
	log.Printf("recv: %#v\n", msg)
	reply := data.Outputting{Message: "worlD!!!!" + msg.From}
	return &reply, nil
}

func main() {

	flag.Parse()
	log.SetFlags(log.Lmicroseconds)

	u := url.URL{Scheme: "ws", Host: *addr, Path: "/ws"}
	log.Printf("connecting to %s", u.String())
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)

	if err != nil {
		log.Fatal("NewClient:", err)
	}

	peer := Peer{}
	registry := jsonrpc.NewRegistry().RegisterService(&peer)
	endpoint := jsonrpc.NewEndpoint(conn, registry)

	go endpoint.Serve()

	defer endpoint.Close()

	start := time.Now()
	reply := data.Outputting{}
	args := data.Incoming{From: "Tom", Message: "hello!"}
	err = endpoint.Call("Chat.Message", &args, &reply)

	if err != nil {
		log.Fatal("Call:", err)
	} else {
		log.Print("recv resp: ", reply)
	}

	log.Print("Elapsed: ", time.Since(start))

	ctrlC := make(chan os.Signal, 1)
	// catch SIGETRM or SIGINTERRUPT
	signal.Notify(ctrlC, syscall.SIGTERM, syscall.SIGINT)

	<-ctrlC
}
