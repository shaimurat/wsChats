package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoDB connection
var chatCollection *mongo.Collection
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// ChatMessage model
type ChatMessage struct {
	ID        string    `bson:"_id,omitempty" json:"id"`
	ChatID    string    `bson:"chatId" json:"chatId"`
	Sender    string    `bson:"sender" json:"sender"`
	Message   string    `bson:"message" json:"message"`
	Timestamp time.Time `bson:"timestamp" json:"timestamp"`
}

// Active WebSocket connections
var clients = make(map[*websocket.Conn]string) // Store user chat sessions
var clientsMutex sync.Mutex

// Admin connections
var adminClients = make(map[*websocket.Conn]bool) // Track connected admins

// Handle WebSocket connections
func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade failed:", err)
		return
	}
	defer ws.Close()

	// Read user details
	var initMsg ChatMessage
	err = ws.ReadJSON(&initMsg)
	if err != nil {
		log.Println("WebSocket Read Error:", err)
		return
	}

	clientsMutex.Lock()
	if initMsg.Sender == "Admin" {
		adminClients[ws] = true // Mark this connection as an admin
	} else {
		clients[ws] = initMsg.ChatID // Store chat session
	}
	clientsMutex.Unlock()

	// Notify client that chat session has started
	ws.WriteJSON(ChatMessage{
		ChatID:  initMsg.ChatID,
		Sender:  "System",
		Message: "A new chat session has been started. Please wait for an admin.",
	})

	for {
		var msg ChatMessage
		err := ws.ReadJSON(&msg)
		if err != nil {
			log.Println("WebSocket Read Error:", err)
			clientsMutex.Lock()
			delete(clients, ws)
			delete(adminClients, ws)
			clientsMutex.Unlock()
			break
		}

		msg.Timestamp = time.Now()
		saveMessage(msg)
		broadcastMessage(msg)
	}
}

// Save message to MongoDB
func saveMessage(msg ChatMessage) {
	_, err := chatCollection.InsertOne(context.TODO(), msg)
	if err != nil {
		log.Println("Error saving message:", err)
	}
}

// Broadcast message to all connected clients
func broadcastMessage(msg ChatMessage) {
	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	for client := range clients {
		err := client.WriteJSON(msg)
		if err != nil {
			log.Println("WebSocket Write Error:", err)
			client.Close()
			delete(clients, client)
		}
	}

	// Send to all admin clients
	for admin := range adminClients {
		err := admin.WriteJSON(msg)
		if err != nil {
			log.Println("WebSocket Write Error:", err)
			admin.Close()
			delete(adminClients, admin)
		}
	}
}

// Fetch chat history
func getChatHistory(c *gin.Context) {
	chatID := c.Param("chatId")

	if chatID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chatId is required"})
		return
	}

	cursor, err := chatCollection.Find(context.TODO(), bson.M{"chatId": chatID})
	if err != nil {
		log.Println("Database error while fetching chat history:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}
	defer cursor.Close(context.TODO())

	var messages []ChatMessage
	for cursor.Next(context.TODO()) {
		var msg ChatMessage
		if err := cursor.Decode(&msg); err != nil {
			log.Println("Error decoding chat message:", err)
			continue
		}
		messages = append(messages, msg)
	}

	// If no chat messages exist, create a system message
	if len(messages) == 0 {
		systemMessage := ChatMessage{
			ChatID:    chatID,
			Sender:    "System",
			Message:   "A new chat session has been started. Please wait for an admin.",
			Timestamp: time.Now(),
		}

		_, err := chatCollection.InsertOne(context.TODO(), systemMessage)
		if err != nil {
			log.Println("Error inserting system message:", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not create chat session"})
			return
		}

		messages = append(messages, systemMessage)
	}

	c.JSON(http.StatusOK, messages)
}

// Get all active user chats
func getActiveChats(c *gin.Context) {
	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	// Debugging: Print all connected clients
	fmt.Println("Debug: Current Active Clients Map:", clients)

	var activeChats []string
	for _, chatID := range clients {
		activeChats = append(activeChats, chatID)
	}

	fmt.Println("Debug: Active Chats:", activeChats)

	// Return active chats, ensuring it's never null
	if len(activeChats) == 0 {
		c.JSON(http.StatusOK, gin.H{"activeChats": []string{}})
	} else {
		c.JSON(http.StatusOK, gin.H{"activeChats": activeChats})
	}
}

// Close an Active Chat
func closeChat(c *gin.Context) {
	chatID := c.Param("chatId")
	if chatID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chatId is required"})
		return
	}

	// Mark the chat as closed in MongoDB
	_, err := chatCollection.UpdateOne(
		context.TODO(),
		bson.M{"chatId": chatID},
		bson.M{"$set": bson.M{"status": "closed"}},
	)
	if err != nil {
		log.Println("Error closing chat:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not close chat"})
		return
	}

	// Remove from active clients
	clientsMutex.Lock()
	defer clientsMutex.Unlock()
	for client, id := range clients {
		if id == chatID {
			client.Close() // Disconnect WebSocket
			delete(clients, client)
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Chat closed successfully"})
}

func main() {
	clientOptions := options.Client().ApplyURI("mongodb+srv://danial:Danial_2005@pokegame.fxobs.mongodb.net/?retryWrites=true&w=majority&appName=PokeGame\"")
	client, err := mongo.Connect(context.TODO(), clientOptions)
	if err != nil {
		log.Fatal(err)
	}
	chatCollection = client.Database("PokeGame").Collection("chats")
	fmt.Println("Chat Service Connected to MongoDB")

	r := gin.Default()
	r.Use(cors.Default())
	r.POST("/closeChat/:chatId", closeChat)
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST"},
		AllowHeaders:     []string{"Origin", "Content-Type"},
		AllowCredentials: true,
	}))

	r.GET("/ws", func(c *gin.Context) {
		handleConnections(c.Writer, c.Request)
	})
	r.GET("/chat/history/:chatId", getChatHistory)
	r.GET("/getActiveChats", getActiveChats) // New API for fetching active chats

	log.Println("Chat Service running on port 8082...")
	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}
	r.Run(":" + port)

}
