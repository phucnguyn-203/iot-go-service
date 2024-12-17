package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/db"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/api/option"
)

type Instruction struct {
	Instruction string `json:"instruction"`
}

type AIResponse struct {
	Target   string `json:"target"`
	Action   string `json:"action"`
	Content  string `json:"content"`
	Location string `json:"location"`
}

var client *db.Client

const (
	lightPrefix   = "light"
	doorPath      = "door/turn"
	databaseURL   = "https://iot-grio9-52213-default-rtdb.asia-southeast1.firebasedatabase.app/"
	aiServiceURL  = "http://localhost:11434/api/generate"
	serviceKey    = "./serviceAccountKey.json"
	actionOn      = "1"
	actionOff     = "0"
	port          = ":3000"
	responseError = "error"
)

func initFirebase() error {
	ctx := context.Background()
	conf := option.WithCredentialsFile(serviceKey)

	app, err := firebase.NewApp(ctx, &firebase.Config{DatabaseURL: databaseURL}, conf)
	if err != nil {
		return errors.Wrap(err, "failed to initialize Firebase app")
	}

	client, err = app.Database(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to initialize Firebase database")
	}
	return nil
}

func handleAPI(c *gin.Context) {
	var inst Instruction
	if err := c.ShouldBindJSON(&inst); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{responseError: "Invalid request payload"})
		return
	}

	aiResponse, err := getAIResponse(inst.Instruction)
	log.Printf("AI Response: { \"target\": \"%s\", \"action\": \"%s\", \"content\": \"%s\", \"location\": \"%s\" }", aiResponse.Target, aiResponse.Action, aiResponse.Content, aiResponse.Location)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{responseError: fmt.Sprintf("Error from AI service: %v", err)})
		return
	}

	processAIResponse(c, aiResponse)
}

func getAIResponse(instruction string) (AIResponse, error) {
	prompt := `When I give you a command, respond with a JSON object that contains the following keys:
		- "target": the target of the action (e.g., "light", "door", etc.).
		- "action": the action to perform (e.g., "on", "off", "open", "close", "play", etc.).
		- "content": the content to search (leave an empty string "" if not specified).
		- "location": the location of the target (e.g., "living room", "bedroom", "toilet", "kitchen", "all", or leave it empty "" if not specified).
		
		Instruction: ` + instruction + `
		
		Example:
		- If the instruction is "turn on the light in the living room", the JSON object should be:
		  {
			"target": "light",
			"action": "on",
			"content": "",
			"location": "living room"
		  }
		- If the instruction is "open the door", the JSON object should be:
		  {
			"target": "door",
			"action": "open",
			"content": "",
			"location": ""
		  }
		- If the instruction is "turn on all the light", the JSON object should be:
			{
				"target": "light",
				"action": "on",
				"content": "",
				"location": "all"
			}
		Please respond with only the JSON format. Do not include any additional explanation or text.`

	payload := map[string]interface{}{
		"model":  "phi3",
		"prompt": fmt.Sprintf("<|system|>You are my Home AI assistant.<|end|><|user|>%s<|end|><|assistant|>", prompt),
		"stream": false,
	}

	resp, err := http.Post("http://localhost:11434/api/generate", "application/json", bytes.NewReader(mustMarshal(payload)))
	if err != nil {
		return AIResponse{}, errors.Wrap(err, "failed to send request to AI service")
	}
	defer resp.Body.Close()

	var data struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return AIResponse{}, errors.Wrap(err, "failed to decode AI response")
	}

	var aiResponse AIResponse
	if err := json.Unmarshal([]byte(data.Response), &aiResponse); err != nil {
		return AIResponse{}, errors.Wrap(err, "failed to parse AI response JSON")
	}
	return aiResponse, nil
}

func processAIResponse(c *gin.Context, response AIResponse) {
	ctx := context.Background()
	action, valid := map[string]string{"on": actionOn, "off": actionOff, "open": actionOn, "close": actionOff}[response.Action]
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{responseError: "Invalid action"})
		return
	}

	switch response.Target {
	case "light":
		if err := updateLight(ctx, response.Location, action); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{responseError: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Light %s in %s", response.Action, response.Location)})
	case "door":
		var isOwner string
		if err := client.NewRef("camera/isOwner").Get(ctx, &isOwner); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{responseError: "Failed to update door status"})
			return
		}
		if isOwner == "1" {
			if err := client.NewRef(doorPath).Set(ctx, action); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{responseError: "Failed to update door status"})
				return
			}
		} else {
			c.JSON(http.StatusOK, gin.H{"message": "You are not the owner"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Door %s", response.Action)})
	default:
		c.JSON(http.StatusBadRequest, gin.H{responseError: "Unsupported target"})
	}
}

func updateLight(ctx context.Context, location, action string) error {
	paths := map[string]string{
		"living room": lightPrefix + "1/turn",
		"bedroom":     lightPrefix + "2/turn",
		"kitchen":     lightPrefix + "3/turn",
		"toilet":      lightPrefix + "4/turn",
		"wc":          lightPrefix + "4/turn",
	}
	if location == "all" {
		for _, path := range paths {
			if err := client.NewRef(path).Set(ctx, action); err != nil {
				return errors.Wrap(err, "failed to update all lights")
			}
		}
		return nil
	}
	path, exists := paths[location]
	if !exists {
		return errors.New("Invalid location")
	}
	return client.NewRef(path).Set(ctx, action)
}

func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		log.Fatalf("failed to marshal payload: %v", err)
	}
	return b
}

func main() {
	if err := initFirebase(); err != nil {
		log.Fatalf("Error initializing Firebase: %v", err)
	}

	r := gin.Default()
	r.POST("/api", handleAPI)

	if err := r.Run(port); err != nil {
		log.Fatalf("Error running server: %v", err)
	}
}
