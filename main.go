package main

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"text/template"
	"time"

	"github.com/garyburd/redigo/redis"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512

	winnerThreshold = 1e8
)

type conn struct {
	ws   *websocket.Conn
	send chan []byte
}

func (c *conn) write(mt int, payload []byte) error {
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteMessage(mt, payload)
}

func (c *conn) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.ws.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				c.ws.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.write(websocket.PingMessage, []byte{}); err != nil {
				return
			}
		}
	}
}

type hub struct {
	conns      map[*conn]bool
	broadcast  chan []byte
	register   chan *conn
	unregister chan *conn
}

func (h *hub) run() {
	for {
		select {
		case c := <-h.register:
			h.conns[c] = true
		case c := <-h.unregister:
			if _, ok := h.conns[c]; ok {
				delete(h.conns, c)
				close(c.send)
			}
		case m := <-h.broadcast:
			for c := range h.conns {
				select {
				case c.send <- m:
				default:
					close(c.send)
					delete(h.conns, c)
				}
			}
		}
	}
}

type msg struct {
	Username string `json:"username"`
	Count    int64  `json:"count"`
	Winner   bool   `json:"winner"`
}

type u_msg struct {
	Username string `json:"username"`
	Count    int64  `json:"count"`
}

func httpLog(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL)
		handler.ServeHTTP(w, r)
		log.Printf("Completed in %s", time.Now().Sub(start).String())
	})
}

func newPool(server, password string) *redis.Pool {
	return &redis.Pool{
		MaxIdle:     3,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			c, err := redis.Dial("tcp", server)
			if err != nil {
				return nil, err
			}
			if password != "" {
				if _, err := c.Do("AUTH", password); err != nil {
					c.Close()
					return nil, err
				}
			}
			return c, err
		},
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			return err
		},
	}
}

func doWithObj(c redis.Conn, action, key string, obj interface{},
	args ...interface{}) (interface{}, error) {
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(obj); err != nil {
		return nil, err
	}

	args = append([]interface{}{key, buf}, args...)

	return c.Do(action, args...)
}

func redisObj(dest, reply interface{}, err error) error {
	buf := bytes.NewBuffer(reply.([]byte))
	if err := gob.NewDecoder(buf).Decode(dest); err != nil {
		return err
	}
	return nil
}

func getMsg(c redis.Conn, key string) (*msg, error) {
	var msg msg
	reply, err := c.Do("GET", key)
	if err != nil {
		return nil, err
	}
	if reply == nil {
		return nil, nil
	}
	if err := redisObj(&msg, reply, err); err != nil {
		return nil, err
	}
	return &msg, nil
}

var (
	h = &hub{
		conns:      make(map[*conn]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *conn),
		unregister: make(chan *conn),
	}

	u = &hub{
		conns:      make(map[*conn]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *conn),
		unregister: make(chan *conn),
	}

	pool *redis.Pool

	count int64
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	redisServer := os.Getenv("REDIS_SERVER")
	redisPassword := os.Getenv("REDIS_PASSWORD")
	if redisServer == "" {
		// Load from Fig environment
		server := os.Getenv("REDIS_1_PORT_6379_TCP_ADDR")
		port := os.Getenv("REDIS_1_PORT_6379_TCP_PORT")
		redisServer = fmt.Sprintf("%s:%s", server, port)
		redisPassword = ""
	}
	pool = newPool(redisServer, redisPassword)

	if err := setInitialRedisValues(); err != nil {
		log.Println(err)
		return
	}

	go h.run()
	go u.run()

	http.HandleFunc("/", serveRoot)
	http.HandleFunc("/user", serveUser)
	http.Handle("/static/",
		http.StripPrefix("/static/", http.FileServer(http.Dir("./static/"))))
	http.HandleFunc("/yo", serveYo)
	http.HandleFunc("/flips", getFlips)
	http.HandleFunc("/connect", serveWs)
	http.HandleFunc("/userconnect", serveUserWs)


	log.Fatal(http.ListenAndServe(":"+port, httpLog(http.DefaultServeMux)))
}

func setInitialRedisValues() error {
	conn := pool.Get()
	defer conn.Close()

	if err := conn.Send("SET", "count", 0, "NX"); err != nil {
		return err
	}

	_, err := doWithObj(conn, "SET", "last_msg", msg{}, "NX")
	if err != nil {
		return err
	}

	return nil
}

func getFlips(w http.ResponseWriter, r *http.Request) {
	conn := pool.Get()
	defer conn.Close()

	msg := &msg{}

	msg.Username = r.URL.Query().Get("username")
    log.Println(msg.Username)
	count, err := redis.Int64(conn.Do("GET", msg.Username))
	if err != nil {
		log.Println(err)
		return
	}
	msg.Count = count
	log.Println(msg)
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Println(err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
  	w.Write(msgBytes)
}

func serveRoot(w http.ResponseWriter, r *http.Request) {
	conn := pool.Get()
	defer conn.Close()

	var msg *msg
	lastMsg, err := getMsg(conn, "last_msg")
	if err != nil {
		log.Println(err)
		return
	}


	msg = lastMsg


	tmpl, err := template.New("index.html").ParseFiles("./index.html")
	if err != nil {
		log.Println(err)
		return
	}

	if err := tmpl.Execute(w, msg); err != nil {
		log.Println(err)
	}
}

func serveUser(w http.ResponseWriter, r *http.Request) {
	conn := pool.Get()
	defer conn.Close()

	msg := &msg{}

	msg.Username = r.URL.Query().Get("username")

	count, err := redis.Int64(conn.Do("GET", msg.Username))
	if err != nil {
		log.Println(err)
		return
	}
	msg.Count = count

	tmpl, err := template.New("index_user.html").ParseFiles("./index_user.html")
	if err != nil {
		log.Println(err)
		return
	}

	if err := tmpl.Execute(w, msg); err != nil {
		log.Println(err)
	}
}

func serveYo(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Auth-Token") != os.Getenv("AUTH_TOKEN") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	conn := pool.Get()
	defer conn.Close()

    // global flip
	msg := &msg{}
	msg.Username = r.URL.Query().Get("username")
	
	count, err := redis.Int64(conn.Do("GET", "count"))
	if err != nil {
		log.Println(err)
		return
	}

	msg.Count = count

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Println(err)
		return
	}
	h.broadcast <- msgBytes

    // broadcast with go routine user flip counts
	u_msg := &u_msg{}
	u_msg.Username = r.URL.Query().Get("username")
	
	u_count, u_err := redis.Int64(conn.Do("GET", u_msg.Username))
	if err != nil {
		log.Println(err)
		return
	}
	u_msg.Count = u_count

	u_msgBytes, u_err := json.Marshal(u_msg)
	if u_err != nil {
		log.Println(u_err)
		return
	}
	u.broadcast <- u_msgBytes


	if _, err := doWithObj(conn, "SET", "last_msg", msg); err != nil {
		log.Println(err)
		return
	}

	if err := conn.Send("INCR", "count"); err != nil {
		log.Println(err)
		return
	}

	if err := conn.Send("INCR", msg.Username); err != nil {
		log.Println(err)
		return
	}
}

func serveWs(w http.ResponseWriter, r *http.Request) {
	redisConn := pool.Get()
	defer redisConn.Close()

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	c := &conn{send: make(chan []byte, 256), ws: ws}
	h.register <- c
	c.writePump()
}

func serveUserWs(w http.ResponseWriter, r *http.Request) {
	redisConn := pool.Get()
	defer redisConn.Close()

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	c := &conn{send: make(chan []byte, 256), ws: ws}
	u.register <- c
	c.writePump()
}
