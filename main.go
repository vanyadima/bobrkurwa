package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024 * 1024 * 10,
	WriteBufferSize: 1024 * 1024 * 10,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type Client struct {
	conn     *websocket.Conn
	username string
	room     string
	inVoice  bool
	muted    bool
	isAdmin  bool
}

type VoiceUser struct {
	Username string `json:"username"`
	Muted    bool   `json:"muted"`
}

type UserRecord struct {
	Username string `json:"username"`
	Password string `json:"password"`
	IsAdmin  bool   `json:"isAdmin"`
}

type Message struct {
	Type       string       `json:"type"`
	Username   string       `json:"username,omitempty"`
	Room       string       `json:"room,omitempty"`
	Content    string       `json:"content,omitempty"`
	Timestamp  string       `json:"timestamp,omitempty"`
	Target     string       `json:"target,omitempty"`
	Payload    interface{}  `json:"payload,omitempty"`
	VoiceUsers []VoiceUser  `json:"voiceUsers,omitempty"`
	Speaking   bool         `json:"speaking,omitempty"`
	Muted      bool         `json:"muted,omitempty"`
	UsersList  []UserRecord `json:"usersList,omitempty"`
	NewUser    *UserRecord  `json:"newUser,omitempty"`
}

var (
	clients     = make(map[*Client]bool)
	clientsMu   sync.Mutex
	db          *sql.DB
	serverToken string
)

func initDB() {
	if err := os.MkdirAll("./data", 0755); err != nil {
		log.Fatal("Ошибка создания папки data:", err)
	}

	var err error
	db, err = sql.Open("sqlite", "./data/koderline.db")
	if err != nil {
		log.Fatal("Ошибка открытия БД:", err)
	}

	_, _ = db.Exec(`PRAGMA journal_mode=WAL;`)

	_, _ = db.Exec(`
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		room TEXT NOT NULL,
		username TEXT NOT NULL,
		msg_type TEXT NOT NULL DEFAULT 'chat',
		content TEXT NOT NULL,
		timestamp TEXT NOT NULL
	);
	`)

	_, _ = db.Exec(`
	CREATE TABLE IF NOT EXISTS users (
		username TEXT PRIMARY KEY,
		password TEXT NOT NULL,
		is_admin BOOLEAN NOT NULL DEFAULT 0
	);
	`)

	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&count)
	if count == 0 {
		adminPass := os.Getenv("ADMIN_PASSWORD")
		if adminPass == "" {
			adminPass = generateRandomPass(8)
			log.Println("==================================================")
			log.Printf(" [!] Автоматически сгенерирован пароль главного админа: %s", adminPass)
			log.Println(" [!] Логин: Admin | Пароль можно сменить в админке")
			log.Println("==================================================")
		}
		_, _ = db.Exec(`INSERT OR REPLACE INTO users (username, password, is_admin) VALUES (?, ?, 1)`, "Admin", adminPass)
	}
}

func generateRandomPass(n int) string {
	bytes := make([]byte, n)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)[:n]
}

func authenticateUser(username, password string) (bool, bool) {
	var dbPass string
	var isAdmin bool
	err := db.QueryRow(`SELECT password, is_admin FROM users WHERE username = ?`, username).Scan(&dbPass, &isAdmin)
	if err != nil {
		return false, false
	}
	return dbPass == password, isAdmin
}

func saveMessage(room, username, msgType, content, timestamp string) {
	_, _ = db.Exec(
		`INSERT INTO messages (room, username, msg_type, content, timestamp) VALUES (?, ?, ?, ?, ?)`,
		room, username, msgType, content, timestamp,
	)
}

func sendRoomHistory(client *Client) {
	rows, err := db.Query(`
		SELECT username, msg_type, content, timestamp FROM (
			SELECT id, username, msg_type, content, timestamp 
			FROM messages 
			WHERE room = ? 
			ORDER BY id DESC 
			LIMIT 50
		) ORDER BY id ASC
	`, client.room)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var msg Message
		msg.Room = client.room
		if err := rows.Scan(&msg.Username, &msg.Type, &msg.Content, &msg.Timestamp); err == nil {
			_ = client.conn.WriteJSON(msg)
		}
	}
}

func sendUsersList(client *Client) {
	if !client.isAdmin {
		return
	}
	rows, err := db.Query(`SELECT username, password, is_admin FROM users`)
	if err != nil {
		return
	}
	defer rows.Close()

	var list []UserRecord
	for rows.Next() {
		var u UserRecord
		if err := rows.Scan(&u.Username, &u.Password, &u.IsAdmin); err == nil {
			list = append(list, u)
		}
	}
	_ = client.conn.WriteJSON(Message{
		Type:      "admin-users",
		UsersList: list,
	})
}

func broadcastVoiceList(room string) {
	clientsMu.Lock()
	var users []VoiceUser
	for c := range clients {
		if c.room == room && c.inVoice {
			users = append(users, VoiceUser{Username: c.username, Muted: c.muted})
		}
	}
	msg := Message{Type: "voice-list", Room: room, VoiceUsers: users}
	for c := range clients {
		if c.room == room {
			_ = c.conn.WriteJSON(msg)
		}
	}
	clientsMu.Unlock()
}

func main() {
	serverToken = os.Getenv("WORKCORD_TOKEN")
	if serverToken == "" {
		serverToken = "admin123"
	}

	initDB()
	defer db.Close()

	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/ws", handleConnections)

	log.Println("Сервер KoderlineWorkSpace запущен на :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token != serverToken {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1024 * 1024 * 10)
	defer ws.Close()

	client := &Client{conn: ws, room: "general"}
	clientsMu.Lock()
	clients[client] = true
	clientsMu.Unlock()

	defer func() {
		room := client.room
		wasInVoice := client.inVoice
		clientsMu.Lock()
		delete(clients, client)
		clientsMu.Unlock()

		if wasInVoice {
			broadcast(Message{Type: "voice-leave", Username: client.username, Room: room}, client)
			broadcastVoiceList(room)
		}
	}()

	for {
		var msg Message
		if err := ws.ReadJSON(&msg); err != nil {
			break
		}

		switch msg.Type {
		case "join":
			isValid, isAdmin := authenticateUser(msg.Username, msg.Content)
			if !isValid && msg.Username != "Admin" {
				_, _ = db.Exec(`INSERT OR IGNORE INTO users (username, password, is_admin) VALUES (?, ?, 0)`, msg.Username, msg.Content)
				isValid, isAdmin = true, false
			}

			client.username = msg.Username
			client.room = msg.Room
			client.isAdmin = isAdmin

			sendRoomHistory(client)
			sendUsersList(client)
			broadcastVoiceList(client.room)

			broadcast(Message{
				Type:      "chat",
				Username:  "Система",
				Room:      client.room,
				Content:   client.username + " подключился к каналу #" + client.room,
				Timestamp: time.Now().Format("15:04"),
			}, nil)

		case "admin-add-user":
			if !client.isAdmin || msg.NewUser == nil || msg.NewUser.Username == "" {
				continue
			}
			_, _ = db.Exec(
				`INSERT OR REPLACE INTO users (username, password, is_admin) VALUES (?, ?, ?)`,
				msg.NewUser.Username, msg.NewUser.Password, msg.NewUser.IsAdmin,
			)
			clientsMu.Lock()
			for c := range clients {
				if c.isAdmin {
					sendUsersList(c)
				}
			}
			clientsMu.Unlock()

		case "admin-del-user":
			if !client.isAdmin || msg.Username == "Admin" {
				continue
			}
			_, _ = db.Exec(`DELETE FROM users WHERE username = ?`, msg.Username)
			clientsMu.Lock()
			for c := range clients {
				if c.isAdmin {
					sendUsersList(c)
				}
			}
			clientsMu.Unlock()

		case "chat", "image":
			msg.Timestamp = time.Now().Format("15:04")
			saveMessage(msg.Room, msg.Username, msg.Type, msg.Content, msg.Timestamp)
			broadcast(msg, nil)

		case "voice-join":
			client.inVoice = true
			client.muted = false
			broadcastToRoom(msg, client)
			broadcastVoiceList(client.room)

		case "voice-leave":
			client.inVoice = false
			client.muted = false
			broadcastToRoom(msg, client)
			broadcastVoiceList(client.room)

		case "mute":
			client.muted = msg.Muted
			broadcastVoiceList(client.room)
			if client.muted {
				broadcast(Message{Type: "speaking", Username: client.username, Room: client.room, Speaking: false}, nil)
			}

		case "speaking":
			if !client.muted {
				broadcast(msg, nil)
			}

		case "webrtc-offer", "webrtc-answer", "webrtc-candidate":
			broadcastToRoom(msg, client)
		}
	}
}

func broadcast(msg Message, exclude *Client) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for c := range clients {
		if c != exclude && c.room == msg.Room {
			_ = c.conn.WriteJSON(msg)
		}
	}
}

func broadcastToRoom(msg Message, sender *Client) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for c := range clients {
		if c == sender || c.room != sender.room {
			continue
		}
		if msg.Target != "" && c.username != msg.Target {
			continue
		}
		_ = c.conn.WriteJSON(msg)
	}
}
