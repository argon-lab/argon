package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Client consent is opt-in; the server accepts only this fixed vocabulary,
// never identifiers, documents, URLs or free-form properties.
func (r *Router) recordEvent(c *gin.Context) {
	var body struct {
		Event string `json:"event"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 256))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid event"})
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		c.JSON(400, gin.H{"error": "invalid event"})
		return
	}
	switch body.Event {
	case "demo_entered", "first_diff", "first_merge", "first_undo", "quickstart_opened":
		log.Printf("argon_funnel event=%s", body.Event)
		c.Status(http.StatusNoContent)
	default:
		c.JSON(400, gin.H{"error": "unknown event"})
	}
}
