package meshtastic

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestWithDefaultPort(t *testing.T) {
	cases := map[string]string{
		"mesh.local":      "mesh.local:4403",
		"mesh.local:4403": "mesh.local:4403",
		"mesh.local:8080": "mesh.local:8080",
		"192.168.1.50":    "192.168.1.50:4403",
		"fd00::1":         "[fd00::1]:4403",
		"[fd00::1]:4403":  "[fd00::1]:4403",
	}
	for in, want := range cases {
		if got := withDefaultPort(in, DefaultTCPPort); got != want {
			t.Errorf("withDefaultPort(%q) = %q, want %q", in, got, want)
		}
	}
}

// The full path a node on WiFi takes: dial, wake, config exchange. The wake
// sequence is deliberately not a frame, so this also proves the radio's parser
// resynchronises after it — which is the point of sending it at all.
func TestDialTCPAndConfigure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		serveRadio(t, conn, answerConfig(configDump))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := DialTCP(ctx, TCPConfig{Host: ln.Addr().String()})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if want := "tcp:" + ln.Addr().String(); c.Name() != want {
		t.Errorf("Name = %q, want %q", c.Name(), want)
	}

	info, err := Configure(ctx, c, ConfigRequest{ID: 1})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if info.NodeNum != 0x11223344 {
		t.Errorf("NodeNum = %#x", info.NodeNum)
	}
}

func TestDialTCPRequiresHost(t *testing.T) {
	if _, err := DialTCP(context.Background(), TCPConfig{}); err == nil {
		t.Fatal("dialled an empty host")
	}
}

// A radio that accepts the connection and then says nothing must not hang the
// caller: over WiFi this is what a node that has crashed but kept its TCP
// stack alive looks like.
func TestDialTCPSilentNode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-make(chan struct{}) // never answers
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	c, err := DialTCP(context.Background(), TCPConfig{Host: ln.Addr().String()})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if _, err := Configure(ctx, c, ConfigRequest{ID: 1}); err == nil {
		t.Fatal("configure returned success from a silent node")
	}
}
