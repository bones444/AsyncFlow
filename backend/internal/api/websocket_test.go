package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
)

func TestWebSocketAuthFirstMessage(t *testing.T) {
	os.Setenv("JWT_SECRET", "test-secret")
	t.Setenv("ALLOWED_ORIGINS", "http://localhost:5173")

	s := &Server{WSHub: NewHub()}
	go s.WSHub.Run()
	srv := httptest.NewServer(http.HandlerFunc(s.HandleWebSocket))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	header := http.Header{"Origin": []string{"http://localhost:5173"}}

	validToken, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": "u1", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("test-secret"))

	t.Run("token in URL no longer authenticates", func(t *testing.T) {
		c, _, err := websocket.DefaultDialer.Dial(url+"?token="+validToken, header)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		c.WriteJSON(map[string]string{"type": "ping"})
		if _, _, err := c.ReadMessage(); err == nil {
			t.Fatal("expected the server to close an unauthenticated connection")
		}
	})

	t.Run("wrong secret is closed", func(t *testing.T) {
		bad, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"user_id": "u1"}).SignedString([]byte("other"))
		c, _, _ := websocket.DefaultDialer.Dial(url, header)
		defer c.Close()
		c.WriteJSON(map[string]string{"type": "auth", "token": bad})
		if _, _, err := c.ReadMessage(); err == nil {
			t.Fatal("expected close for bad signature")
		}
	})

	t.Run("valid first message receives broadcast", func(t *testing.T) {
		c, _, err := websocket.DefaultDialer.Dial(url, header)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		c.WriteJSON(map[string]string{"type": "auth", "token": validToken})
		time.Sleep(100 * time.Millisecond) // registration happens after auth
		s.WSHub.Broadcast <- []byte(`{"type":"NEW_POLL_VOTE"}`)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil || string(msg) != `{"type":"NEW_POLL_VOTE"}` {
			t.Fatalf("broadcast not received: %v %q", err, msg)
		}
	})

	t.Run("disallowed browser origin is rejected", func(t *testing.T) {
		_, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Origin": []string{"https://evil.example"}})
		if err == nil {
			t.Fatal("expected handshake to fail for disallowed origin")
		}
		if resp != nil && resp.StatusCode == http.StatusSwitchingProtocols {
			t.Fatal("upgrade must not succeed")
		}
	})
}
