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
	"github.com/google/uuid"
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

// Chat model
type Chat struct {
	ID        string        `bson:"_id,omitempty" json:"id"`
	ChatID    string        `bson:"chatId" json:"chatId"`
	UserEmail string        `bson:"userEmail" json:"userEmail"`
	Messages  []ChatMessage `bson:"messages" json:"messages"`
	Status    string        `bson:"status" json:"status"` // "active" or "ended"
}

// ChatMessage model
type ChatMessage struct {
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

	// Read initial message to get user details
	var initMsg struct {
		ChatID    string `json:"chatId"`
		UserEmail string `json:"userEmail"`
		Sender    string `json:"sender"`
	}
	fmt.Println(initMsg.UserEmail)
	err = ws.ReadJSON(&initMsg)
	if err != nil {
		log.Println("WebSocket Read Error:", err)
		return
	}

	// If no chat ID is provided, create a new one
	if initMsg.ChatID == "" {
		initMsg.ChatID = uuid.New().String()
	}

	// Ensure chat exists in DB before proceeding
	filter := bson.M{"chatId": initMsg.ChatID}
	update := bson.M{
		"$setOnInsert": bson.M{
			"chatId":    initMsg.ChatID,
			"userEmail": initMsg.UserEmail,
			"messages":  []ChatMessage{}, // Initialize messages as empty array
			"status":    "active",
		},
	}
	options := options.Update().SetUpsert(true)
	_, err = chatCollection.UpdateOne(context.TODO(), filter, update, options)
	if err != nil {
		log.Println("Error ensuring chat exists:", err)
		return
	}

	clientsMutex.Lock()
	clients[ws] = initMsg.ChatID // Store the chat session
	clientsMutex.Unlock()

	// Notify client that chat session has started
	ws.WriteJSON(ChatMessage{
		Sender:    "System",
		Message:   "Chat session started.",
		Timestamp: time.Now(),
	})

	// Listen for messages
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
		saveMessage(initMsg.ChatID, msg)
		broadcastMessage(initMsg.ChatID, msg)
	}
}

// Save message to MongoDB by appending to the messages array
// Save message to MongoDB by appending to the messages array
func saveMessage(chatID string, msg ChatMessage) {
	filter := bson.M{"chatId": chatID}
	update := bson.M{
		"$push":        bson.M{"messages": msg},    // Append message to messages array
		"$setOnInsert": bson.M{"status": "active"}, // Set status only if inserting new doc
	}

	// Use upsert: true to create chat if it doesn’t exist
	options := options.Update().SetUpsert(true)

	_, err := chatCollection.UpdateOne(context.TODO(), filter, update, options)
	if err != nil {
		log.Println("Error saving message:", err)
	}
}

// Broadcast message to all connected clients
func broadcastMessage(chatID string, msg ChatMessage) {
	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	for client, id := range clients {
		if id == chatID {
			err := client.WriteJSON(msg)
			if err != nil {
				log.Println("WebSocket Write Error:", err)
				client.Close()
				delete(clients, client)
			}
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

// Fetch chat history by chatId
func getChatHistory(c *gin.Context) {
	chatID := c.Param("chatId")

	if chatID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chatId is required"})
		return
	}

	var chat Chat
	err := chatCollection.FindOne(context.TODO(), bson.M{"chatId": chatID}).Decode(&chat)
	if err != nil {
		log.Println("Database error while fetching chat history:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	c.JSON(http.StatusOK, chat.Messages)
}

// Get all chats by user email
func getUserChats(c *gin.Context) {
	userEmail := c.Param("userEmail")

	if userEmail == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "userEmail is required"})
		return
	}

	cursor, err := chatCollection.Find(context.TODO(), bson.M{"userEmail": userEmail})
	if err != nil {
		log.Println("Database error while fetching user chats:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}
	defer cursor.Close(context.TODO())

	var chats []Chat
	for cursor.Next(context.TODO()) {
		var chat Chat
		if err := cursor.Decode(&chat); err != nil {
			log.Println("Error decoding chat:", err)
			continue
		}
		chats = append(chats, chat)
	}

	c.JSON(http.StatusOK, chats)
}

// Close an active chat (Update status to "ended")
// Закрытие активного чата (Обновляем статус и уведомляем пользователей)
func closeChat(c *gin.Context) {
	chatID := c.Param("chatId")
	if chatID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chatId is required"})
		return
	}

	// Обновляем статус чата на "ended"
	filter := bson.M{"chatId": chatID}
	update := bson.M{"$set": bson.M{"status": "ended"}}

	_, err := chatCollection.UpdateOne(context.TODO(), filter, update)
	if err != nil {
		log.Println("Error closing chat:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not close chat"})
		return
	}

	// Оповещение всех клиентов о закрытии чата
	clientsMutex.Lock()
	defer clientsMutex.Unlock()
	for client, id := range clients {
		if id == chatID {
			closeMessage := ChatMessage{
				Sender:    "System",
				Message:   "Chat closed by admin.",
				Timestamp: time.Now(),
			}
			err := client.WriteJSON(closeMessage)
			if err != nil {
				log.Println("WebSocket Write Error:", err)
			}
			client.Close() // Закрываем WebSocket соединение
			delete(clients, client)
		}
	}

	// Удаляем соединение у админа, если он был подключен
	for admin, isAdmin := range adminClients {
		if isAdmin {
			admin.Close()
			delete(adminClients, admin)
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "Chat closed successfully"})
}

// Get only active chats for a user
func getUserActiveChats(c *gin.Context) {
	userEmail := c.Param("userEmail")

	if userEmail == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "userEmail is required"})
		return
	}

	// Find only active chats
	cursor, err := chatCollection.Find(context.TODO(), bson.M{"userEmail": userEmail, "status": "active"})
	if err != nil {
		log.Println("Database error while fetching user active chats:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}
	defer cursor.Close(context.TODO())

	var chats []Chat
	for cursor.Next(context.TODO()) {
		var chat Chat
		if err := cursor.Decode(&chat); err != nil {
			log.Println("Error decoding chat:", err)
			continue
		}
		chats = append(chats, chat)
	}

	c.JSON(http.StatusOK, chats)
}
func getActiveChats(c *gin.Context) {
	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	// Debugging: Print all connected clients
	fmt.Println("Debug: Current Active Clients Map:", clients)

	var activeChats []string
	for _, chatID := range clients {
		// Проверяем статус чата в MongoDB
		var chat Chat
		err := chatCollection.FindOne(context.TODO(), bson.M{"chatId": chatID, "status": "active"}).Decode(&chat)
		if err == nil { // Чат найден и активен
			activeChats = append(activeChats, chatID)
		} else {
			fmt.Println("Chat is not active or not found:", chatID) // Debugging
		}
	}

	fmt.Println("Debug: Active Chats:", activeChats)

	// Return only active chats
	c.JSON(http.StatusOK, gin.H{"activeChats": activeChats})
}

// Register route in main function

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

	r.GET("/ws", func(c *gin.Context) {
		handleConnections(c.Writer, c.Request)
	})
	r.POST("/closeChat/:chatId", closeChat)

	r.GET("/chat/history/:chatId", getChatHistory)
	r.GET("/user/chats/:userEmail", getUserChats) // Fetch user chats
	r.GET("/user/activeChats/:userEmail", getUserActiveChats)
	r.GET("/getActiveChats", getActiveChats)
	log.Println("Chat Service running on port 8082...")
	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}
	r.Run(":" + port)
}
