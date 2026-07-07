// The console read surface and cross-cutting server options. These
// endpoints exist for the web console (and any read-heavy client): they
// expose what the CLI can already show — history, state at an LSN,
// persisted merge plans, the sandbox lifecycle — without adding semantics.

package server

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Version is stamped at build time via -ldflags
// "-X github.com/argon-lab/argon/api/server.Version=...". It is what
// /api/v1/meta reports.
var Version = "dev"

// Options are the cross-cutting server settings. The zero value is an open
// local control plane: any origin, no token, writes allowed.
type Options struct {
	// CORSOrigins is a comma-separated allowlist of browser origins;
	// empty or "*" allows any origin.
	CORSOrigins string
	// Token, when set, requires "Authorization: Bearer <token>" on every
	// endpoint except /health and /api/v1/meta.
	Token string
	// ReadOnly rejects every non-GET request. The web console uses it to
	// serve a look-but-don't-touch instance.
	ReadOnly bool
	// Version is reported by /api/v1/meta.
	Version string
}

// OptionsFromEnv reads the server options from the environment:
// ARGON_CORS_ORIGINS, ARGON_API_TOKEN, ARGON_READ_ONLY.
func OptionsFromEnv() Options {
	readOnly := false
	switch strings.ToLower(os.Getenv("ARGON_READ_ONLY")) {
	case "1", "true", "yes":
		readOnly = true
	}
	return Options{
		CORSOrigins: os.Getenv("ARGON_CORS_ORIGINS"),
		Token:       os.Getenv("ARGON_API_TOKEN"),
		ReadOnly:    readOnly,
		Version:     Version,
	}
}

// --- middleware ---

func corsMiddleware(origins string) gin.HandlerFunc {
	allowAll := origins == "" || origins == "*"
	allowed := make(map[string]bool)
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return func(c *gin.Context) {
		if origin := c.GetHeader("Origin"); origin != "" && (allowAll || allowed[origin]) {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
			c.Header("Access-Control-Max-Age", "600")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func authMiddleware(token string) gin.HandlerFunc {
	want := []byte("Bearer " + token)
	return func(c *gin.Context) {
		// Only the API is guarded: /health stays open for probes, static
		// console assets are public (the data behind them is not), and
		// /meta lets a client discover it must present a token.
		p := c.Request.URL.Path
		if !strings.HasPrefix(p, "/api/") || p == "/api/v1/meta" {
			c.Next()
			return
		}
		if subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), want) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid bearer token"})
			return
		}
		c.Next()
	}
}

func readOnlyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead:
			c.Next()
		default:
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "server is read-only"})
		}
	}
}

// --- helpers ---

func intQuery(c *gin.Context, name string, def int64) (int64, error) {
	v := c.Query(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", name, v)
	}
	return n, nil
}

// --- meta / status ---

func (r *Router) meta(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version":   r.opts.Version,
		"read_only": r.opts.ReadOnly,
	})
}

func (r *Router) ingesterStatus(c *gin.Context) {
	r.ingestMu.Lock()
	ids := make([]string, 0, len(r.ingest))
	for id := range r.ingest {
		ids = append(ids, id)
	}
	r.ingestMu.Unlock()
	sort.Strings(ids)
	c.JSON(http.StatusOK, gin.H{"ingesters": ids, "count": len(ids)})
}

// --- history ---

func (r *Router) listEntries(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	filter := bson.M{"branch_id": branchID}
	lsnRange := bson.M{}
	fromLSN, err := intQuery(c, "from_lsn", 0)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	if fromLSN > 0 {
		lsnRange["$gte"] = fromLSN
	}
	toLSN, err := intQuery(c, "to_lsn", 0)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	if toLSN > 0 {
		lsnRange["$lte"] = toLSN
	}
	if len(lsnRange) > 0 {
		filter["lsn"] = lsnRange
	}
	if actor := c.Query("actor"); actor != "" {
		filter["actor"] = actor
	}
	if collection := c.Query("collection"); collection != "" {
		filter["collection"] = collection
	}

	limit, err := intQuery(c, "limit", 50)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	if limit < 1 || limit > 500 {
		limit = 500
	}
	order := -1 // newest first: the timeline reads backwards
	if c.Query("order") == "asc" {
		order = 1
	}

	// One extra row answers "is there another page" without a count.
	opts := options.Find().
		SetSort(bson.D{{Key: "lsn", Value: order}}).
		SetLimit(limit + 1)
	entries, err := r.services.WAL.GetEntries(filter, opts)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	hasMore := false
	if int64(len(entries)) > limit {
		hasMore = true
		entries = entries[:limit]
	}
	c.JSON(http.StatusOK, gin.H{"entries": entries, "has_more": hasMore})
}

// --- time travel reads ---

func (r *Router) timeTravelQuery(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	branch, err := r.services.Branches.GetBranchByID(branchID)
	if err != nil {
		abortErr(c, http.StatusNotFound, err)
		return
	}
	lsn, err := intQuery(c, "lsn", branch.HeadLSN)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}

	collection := c.Query("collection")
	if collection == "" {
		// No collection: a summary of the branch at that LSN.
		state, err := r.services.TimeTravel.GetBranchStateAtLSN(branch, lsn)
		if err != nil {
			abortErr(c, http.StatusBadRequest, err)
			return
		}
		counts := make(map[string]int, len(state))
		for name, docs := range state {
			counts[name] = len(docs)
		}
		c.JSON(http.StatusOK, gin.H{"lsn": lsn, "collections": counts})
		return
	}

	docsByID, err := r.services.TimeTravel.MaterializeAtLSN(branch, collection, lsn)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	skip, err := intQuery(c, "skip", 0)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	limit, err := intQuery(c, "limit", 50)
	if err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	if limit < 1 || limit > 500 {
		limit = 500
	}

	ids := make([]string, 0, len(docsByID))
	for id := range docsByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if skip < 0 {
		skip = 0
	}
	if skip > int64(len(ids)) {
		skip = int64(len(ids))
	}
	end := skip + limit
	if end > int64(len(ids)) {
		end = int64(len(ids))
	}
	documents := make([]bson.M, 0, end-skip)
	for _, id := range ids[skip:end] {
		documents = append(documents, docsByID[id])
	}
	c.JSON(http.StatusOK, gin.H{
		"lsn":        lsn,
		"collection": collection,
		"total":      len(docsByID),
		"documents":  documents,
	})
}

// --- merge plans (read side) ---

func (r *Router) listMergePlans(c *gin.Context) {
	name := c.Query("project")
	if name == "" {
		abortErr(c, http.StatusBadRequest, fmt.Errorf("project query parameter is required"))
		return
	}
	project, err := r.services.Projects.GetProjectByName(name)
	if err != nil {
		abortErr(c, http.StatusNotFound, fmt.Errorf("project %q not found", name))
		return
	}
	plans, err := r.services.Merge.ListPlans(c.Request.Context(), project.ID)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"plans": plans})
}

func (r *Router) getMergePlan(c *gin.Context) {
	planID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		abortErr(c, http.StatusBadRequest, fmt.Errorf("invalid plan id"))
		return
	}
	plan, err := r.services.Merge.GetPlan(c.Request.Context(), planID)
	if err != nil {
		abortErr(c, http.StatusNotFound, err)
		return
	}
	c.JSON(http.StatusOK, plan)
}

// --- sandbox lifecycle (beyond create) ---

func (r *Router) listSandboxes(c *gin.Context) {
	projectID, _, ok := r.resolve(c)
	if !ok {
		return
	}
	boxes, err := r.services.Sandbox.ListSandboxes(c.Request.Context(), projectID)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	items := make([]gin.H, 0, len(boxes))
	for _, b := range boxes {
		item := gin.H{"branch": b}
		if b.IsLive() {
			item["connection_string"] = r.services.BranchConnectionString(b.PhysicalDB)
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"sandboxes": items})
}

func (r *Router) discardSandbox(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	r.stopIngester(branchID)
	if err := r.services.Sandbox.Discard(c.Request.Context(), branchID); err != nil {
		abortErr(c, http.StatusConflict, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"discarded": true})
}

func (r *Router) extendSandbox(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	var body struct {
		TTLMinutes float64 `json:"ttl_minutes" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		abortErr(c, http.StatusBadRequest, err)
		return
	}
	expires, err := r.services.Sandbox.Extend(c.Request.Context(), branchID, time.Duration(body.TTLMinutes)*time.Minute)
	if err != nil {
		abortErr(c, http.StatusConflict, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"expires_at": expires})
}

func (r *Router) keepSandbox(c *gin.Context) {
	_, branchID, ok := r.resolve(c)
	if !ok {
		return
	}
	if err := r.services.Sandbox.Keep(c.Request.Context(), branchID); err != nil {
		abortErr(c, http.StatusConflict, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"kept": true})
}
