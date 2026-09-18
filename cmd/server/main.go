// Command server 协作文档编辑服务主程序
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/google/uuid"

	"coedit/internal/api"
	"coedit/internal/store"
	"coedit/internal/ws"
)

var (
	port     = flag.String("port", "8080", "HTTP server port")
	dbHost   = flag.String("db-host", "localhost", "MySQL host")
	dbPort   = flag.Int("db-port", 3306, "MySQL port")
	dbUser   = flag.String("db-user", "root", "MySQL user")
	dbPass   = flag.String("db-pass", "root", "MySQL password")
	dbName   = flag.String("db-name", "coedit", "MySQL database name")
)

func main() {
	flag.Parse()

	// 连接数据库
	db, err := store.NewDB(store.Config{
		Host:     *dbHost,
		Port:     *dbPort,
		User:     *dbUser,
		Password: *dbPass,
		Database: *dbName,
	})
	if err != nil {
		log.Fatalf("Failed to connect MySQL: %v", err)
	}
	defer db.Close()
	log.Println("MySQL connected successfully")

	// 创建store和hub
	st := store.NewStore(db)
	hub := ws.NewHub()
	go hub.Run()

	server := api.NewServer(db, st, hub)

	// 创建默认文档
	if err := ensureDefaultDoc(st); err != nil {
		log.Printf("Warning: ensure default doc failed: %v", err)
	}

	// 路由设置
	mux := http.NewServeMux()

	// 静态文件
	mux.Handle("/", http.FileServer(http.Dir("web/static")))

	// API路由
	mux.HandleFunc("/api/documents", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			server.ListDocuments(w, r)
		case "POST":
			server.CreateDocument(w, r)
		default:
			http.Error(w, "method not allowed", 405)
		}
	})

	mux.HandleFunc("/api/documents/", func(w http.ResponseWriter, r *http.Request) {
		// 子路由: /api/documents/{id}, /api/documents/{id}/snapshot, etc.
		path := r.URL.Path[len("/api/documents/"):]

		switch {
		case path == "":
			server.ListDocuments(w, r)
			return
		case hasSuffix(path, "/snapshot"):
			docID := path[:len(path)-len("/snapshot")]
			server.GetDocumentSnapshotByID(w, r, docID)
			return
		case hasSuffix(path, "/operations"):
			docID := path[:len(path)-len("/operations")]
			server.GetOperationsByID(w, r, docID)
			return
		case hasSuffix(path, "/versions"):
			docID := path[:len(path)-len("/versions")]
			server.GetVersionHistoryByID(w, r, docID)
			return
		case hasSuffix(path, "/rollback"):
			docID := path[:len(path)-len("/rollback")]
			server.RollbackDocumentByID(w, r, docID)
			return
		default:
			switch r.Method {
			case "GET":
				server.GetDocumentByID(w, r, path)
			case "DELETE":
				server.DeleteDocumentByID(w, r, path)
			default:
				http.Error(w, "method not allowed", 405)
			}
		}
	})

	// WebSocket路由
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		server.HandleWebSocket(w, r)
	})

	// 健康检查
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	addr := fmt.Sprintf(":%s", *port)
	log.Printf("CoEdit server starting on %s", addr)
	log.Printf("Open http://localhost%s in your browser", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// ensureDefaultDoc 确保有一个默认文档
func ensureDefaultDoc(st *store.Store) error {
	docs, err := st.ListDocuments()
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		id := uuid.New().String()
		_, err = st.CreateDocument(id, "欢迎使用协同编辑器")
		if err != nil {
			return err
		}
		log.Printf("Created default document: %s", id)
	}
	return nil
}
