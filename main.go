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
	ID          string        `bson:"_id,omitempty" json:"id"`
	ChatID      string        `bson:"chatId" json:"chatId"`
	UserEmail   string        `bson:"userEmail" json:"userEmail"`
	Messages    []ChatMessage `bson:"messages" json:"messages"`
	LastMessage ChatMessage   `bson:"lastMessage" json:"lastMessage"`
	Status      string        `bson:"status" json:"status"` // "active" or "ended"
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
	}

	err = ws.ReadJSON(&initMsg)
	if err != nil {
		log.Println("WebSocket Read Error:", err)
		return
	}

	// Generate a new chat ID if not provided
	if initMsg.ChatID == "" {
		initMsg.ChatID = uuid.New().String()
	}

	// Проверяем текущий статус чата
	var existingChat Chat
	err = chatCollection.FindOne(context.TODO(), bson.M{"chatId": initMsg.ChatID}).Decode(&existingChat)
	if err != nil && err != mongo.ErrNoDocuments {
		log.Println("Error fetching chat status:", err)
		return
	}

	// Если чат существует и он "ended", не позволяем его снова активировать
	if existingChat.Status == "ended" {
		log.Println("Chat is closed, rejecting connection")
		ws.WriteJSON(ChatMessage{
			Sender:    "System",
			Message:   "This chat has been closed by the admin.",
			Timestamp: time.Now(),
		})
		return
	}

	// Ensure chat exists, but НЕ обновляем статус, если он "ended"
	filter := bson.M{"chatId": initMsg.ChatID}
	update := bson.M{
		"$setOnInsert": bson.M{
			"userEmail": initMsg.UserEmail,
			"messages":  []ChatMessage{},
			"status":    "active", // Только при создании нового чата
		},
	}

	options := options.Update().SetUpsert(true)
	_, err = chatCollection.UpdateOne(context.TODO(), filter, update, options)
	if err != nil {
		log.Println("Error ensuring chat exists:", err)
		return
	}

	clientsMutex.Lock()
	clients[ws] = initMsg.ChatID
	clientsMutex.Unlock()

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
			clientsMutex.Unlock()
			break
		}

		msg.Timestamp = time.Now()
		saveMessage(initMsg.ChatID, msg)
		broadcastMessage(initMsg.ChatID, msg)
	}
}

// Save message to MongoDB by appending to the messages array
func saveMessage(chatID string, msg ChatMessage) {
	filter := bson.M{"chatId": chatID}
	update := bson.M{
		"$push": bson.M{"messages": msg},
		"$set": bson.M{"lastMessageTime": msg.Timestamp,
			"lastMessage": msg,
		}, // Append message to messages array
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

// Get active chats for a user
func getUserActiveChats(c *gin.Context) {
	userEmail := c.Param("userEmail")

	if userEmail == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "userEmail is required"})
		return
	}

	cursor, err := chatCollection.Find(context.TODO(), bson.M{"userEmail": userEmail, "status": "active"})
	if err != nil {
		log.Println("Database error while fetching user active chats:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}
	defer cursor.Close(context.TODO())

	var activeChats []Chat
	for cursor.Next(context.TODO()) {
		var chat Chat
		if err := cursor.Decode(&chat); err != nil {
			log.Println("Error decoding chat:", err)
			continue
		}
		activeChats = append(activeChats, chat)
	}

	c.JSON(http.StatusOK, gin.H{"activeChats": activeChats})
}

// Close an Active Chat
func closeChat(c *gin.Context) {
	chatID := c.Param("chatId")
	if chatID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chatId is required"})
		return
	}

	// Update the chat status to "ended" in MongoDB
	filter := bson.M{"chatId": chatID}
	update := bson.M{"$set": bson.M{"status": "ended"}}

	_, err := chatCollection.UpdateOne(context.TODO(), filter, update)
	if err != nil {
		log.Println("Error closing chat:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not close chat"})
		return
	}

	// Notify all users/admins in this chat
	closeMessage := ChatMessage{
		Sender:    "System",
		Message:   "This chat has been closed by the admin. Please refresh the Page",
		Timestamp: time.Now(),
	}

	broadcastMessage(chatID, closeMessage)

	// Remove the chat session from active clients
	clientsMutex.Lock()
	for client, id := range clients {
		if id == chatID {
			client.Close() // Close WebSocket connection
			delete(clients, client)
		}
	}
	clientsMutex.Unlock()

	c.JSON(http.StatusOK, gin.H{"message": "Chat closed successfully"})
}

// Get all active chats with user emails
func getActiveChats(c *gin.Context) {
	cursor, err := chatCollection.Find(context.TODO(), bson.M{"status": "active"})
	if err != nil {
		log.Println("Database error while fetching active chats:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}
	defer cursor.Close(context.TODO())

	var activeChats []Chat
	for cursor.Next(context.TODO()) {
		var chat Chat
		if err := cursor.Decode(&chat); err != nil {
			log.Println("Error decoding chat:", err)
			continue
		}
		activeChats = append(activeChats, chat)
	}

	c.JSON(http.StatusOK, gin.H{"activeChats": activeChats})
}

// Get ended chats for a user
func getUserEndedChats(c *gin.Context) {
	userEmail := c.Param("userEmail")
	userStatus := c.Query("userStatus") // Используем Query-параметр вместо Param

	if userEmail == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "userEmail is required"})
		return
	}

	var cursor *mongo.Cursor
	var err error

	// Проверяем статус пользователя
	if userStatus == "admin" {
		cursor, err = chatCollection.Find(context.TODO(), bson.M{"status": "ended"})
	} else {
		cursor, err = chatCollection.Find(context.TODO(), bson.M{"userEmail": userEmail, "status": "ended"})
	}

	if err != nil {
		log.Println("Database error while fetching ended chats:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}
	defer cursor.Close(context.TODO())

	var endedChats []Chat
	for cursor.Next(context.TODO()) {
		var chat Chat
		if err := cursor.Decode(&chat); err != nil {
			log.Println("Error decoding chat:", err)
			continue
		}
		endedChats = append(endedChats, chat)
	}

	c.JSON(http.StatusOK, gin.H{"endedChats": endedChats})
}

func main() {
	clientOptions := options.Client().ApplyURI("mongodb+srv://danial:Danial_2005@pokegame.fxobs.mongodb.net/?retryWrites=true&w=majority&appName=PokeGame")
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
	r.GET("/getActiveChats", getActiveChats)
	r.GET("/chat/history/:chatId", getChatHistory)
	r.GET("/user/activeChats/:userEmail", getUserActiveChats)
	r.GET("/user/endedChats/:userEmail/:userStatus", getUserEndedChats)

	r.POST("/closeChat/:chatId", closeChat)
	log.Println("Chat Service running on port 8082...")
	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}
	r.Run(":" + port)
}
