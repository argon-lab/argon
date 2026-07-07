// The hosted-demo gateway. One env switch (ARGON_DEMO_MODE=1) turns the
// server into an anonymous playground: every visitor gets one ephemeral
// seeded project, requests are scoped to it, writes are rate-limited,
// and a sweeper reclaims projects past their TTL. Sessions are
// stateless — the cookie carries the project name (48 random bits), so
// isolation survives restarts and needs no server-side session store.

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func parseObjectID(id string) (primitive.ObjectID, error) {
	return primitive.ObjectIDFromHex(id)
}

const demoCookie = "argon_demo"

var demoName = regexp.MustCompile(`^demo-[0-9a-f]{12}$`)

// demoState is the gateway's only mutable state: a per-session write
// rate limiter. Everything else lives in the engine.
type demoState struct {
	mu      sync.Mutex
	windows map[string]*writeWindow
	cancel  context.CancelFunc
}

type writeWindow struct {
	start time.Time
	count int
}

// allowWrite counts a write against the session's one-minute window.
func (d *demoState) allowWrite(key string, limit int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := d.windows[key]
	now := time.Now()
	if w == nil || now.Sub(w.start) > time.Minute {
		w = &writeWindow{start: now}
		d.windows[key] = w
	}
	if w.count >= limit {
		return false
	}
	w.count++
	// Opportunistic pruning keeps the map from growing unbounded.
	if len(d.windows) > 4096 {
		for k, win := range d.windows {
			if now.Sub(win.start) > time.Minute {
				delete(d.windows, k)
			}
		}
	}
	return true
}

// demoProject resolves the visitor's project from the cookie, or ""
// when there is no valid session.
func (r *Router) demoProject(c *gin.Context) string {
	cookie, err := c.Cookie(demoCookie)
	if err != nil || !demoName.MatchString(cookie) {
		return ""
	}
	if _, err := r.services.Projects.GetProjectByName(cookie); err != nil {
		return "" // swept or never existed
	}
	return cookie
}

// demoGuard scopes every API request to the visitor's own project.
func (r *Router) demoGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		if !strings.HasPrefix(p, "/api/") || p == "/api/v1/meta" {
			c.Next()
			return
		}

		// Writes are bounded before anything else: small bodies, and a
		// per-session-per-minute budget.
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
			key := c.ClientIP()
			if cookie, err := c.Cookie(demoCookie); err == nil {
				key += "|" + cookie
			}
			if !r.demo.allowWrite(key, r.opts.DemoWriteLimit) {
				c.AbortWithStatusJSON(http.StatusTooManyRequests,
					gin.H{"error": "demo write budget exhausted; try again in a minute"})
				return
			}
		}

		if p == "/api/v1/demo/session" {
			c.Next()
			return
		}

		project := r.demoProject(c)
		if project == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"error": "no demo session; POST /api/v1/demo/session first"})
			return
		}

		switch {
		// The project list is the visitor's project, nothing else.
		case p == "/api/v1/projects" && c.Request.Method == http.MethodGet:
			proj, err := r.services.Projects.GetProjectByName(project)
			if err != nil {
				abortErr(c, http.StatusInternalServerError, err)
				return
			}
			c.AbortWithStatusJSON(http.StatusOK, gin.H{"projects": []interface{}{proj}})
			return
		case p == "/api/v1/projects" && c.Request.Method == http.MethodPost:
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"error": "the demo provisions one project per session"})
			return
		// Merge plans are reached by ID; verify ownership before the
		// handler runs.
		case strings.HasPrefix(p, "/api/v1/merge-plans"):
			if name := c.Query("project"); name != "" && name != project {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			if id := c.Param("id"); id != "" {
				if !r.demoOwnsPlan(c, project, id) {
					c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "not found"})
					return
				}
			}
		// Ingester status narrows to the visitor's branches.
		case p == "/api/v1/status/ingesters":
			r.demoIngesterStatus(c, project)
			return
		// Everything project-scoped must be the visitor's project.
		default:
			if name := c.Param("project"); name != "" && name != project {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
		}
		c.Next()
	}
}

func (r *Router) demoOwnsPlan(c *gin.Context, project, planID string) bool {
	proj, err := r.services.Projects.GetProjectByName(project)
	if err != nil {
		return false
	}
	oid, err := parseObjectID(planID)
	if err != nil {
		return true // let the handler produce its own 400
	}
	plan, err := r.services.Merge.GetPlan(c.Request.Context(), oid)
	if err != nil {
		return false
	}
	return plan.ProjectID == proj.ID
}

func (r *Router) demoIngesterStatus(c *gin.Context, project string) {
	proj, err := r.services.Projects.GetProjectByName(project)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	r.ingestMu.Lock()
	ids := make([]string, 0, len(r.ingest))
	for id := range r.ingest {
		ids = append(ids, id)
	}
	r.ingestMu.Unlock()
	mine := make([]string, 0, len(ids))
	for _, id := range ids {
		if b, err := r.services.Branches.GetBranchByID(id); err == nil && b.ProjectID == proj.ID {
			mine = append(mine, id)
		}
	}
	c.AbortWithStatusJSON(http.StatusOK, gin.H{"ingesters": mine, "count": len(mine)})
}

// --- session ---

// demoSession returns the visitor's project, creating and seeding one on
// first contact.
func (r *Router) demoSession(c *gin.Context) {
	if project := r.demoProject(c); project != "" {
		proj, _ := r.services.Projects.GetProjectByName(project)
		c.JSON(http.StatusOK, gin.H{
			"project":    project,
			"expires_at": proj.CreatedAt.Add(r.opts.DemoTTL),
		})
		return
	}

	projects, err := r.services.Projects.ListProjects()
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	live := 0
	for _, p := range projects {
		if demoName.MatchString(p.Name) {
			live++
		}
	}
	if live >= r.opts.DemoMaxProjects {
		c.AbortWithStatusJSON(http.StatusTooManyRequests,
			gin.H{"error": "the demo is at capacity; try again shortly"})
		return
	}

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	name := "demo-" + hex.EncodeToString(suffix)
	project, err := r.services.Projects.CreateProject(name)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := r.seedDemo(c.Request.Context(), name); err != nil {
		_ = r.services.Projects.DeleteProject(project.ID)
		abortErr(c, http.StatusInternalServerError, fmt.Errorf("failed to seed demo project: %w", err))
		return
	}

	maxAge := int(r.opts.DemoTTL / time.Second)
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(demoCookie, name, maxAge, "/", "", false, true)
	c.JSON(http.StatusCreated, gin.H{
		"project":    name,
		"expires_at": project.CreatedAt.Add(r.opts.DemoTTL),
	})
}

// seedDemo writes a small storefront onto main and pins the result, so
// diff, time travel and undo have material from the first click.
func (r *Router) seedDemo(ctx context.Context, project string) error {
	w, err := r.services.WriterFor(project, "main")
	if err != nil {
		return err
	}
	w.SetActor("user:demo")
	docs := []struct {
		collection string
		doc        bson.M
	}{
		{"products", bson.M{"_id": "keyboard", "name": "Keyboard MX", "price": 89, "stock": 40}},
		{"products", bson.M{"_id": "mouse", "name": "Mouse S1", "price": 49, "stock": 65}},
		{"products", bson.M{"_id": "monitor", "name": "Monitor 27q", "price": 329, "stock": 12}},
		{"orders", bson.M{"_id": "o1", "product": "keyboard", "qty": 2, "status": "pending"}},
		{"orders", bson.M{"_id": "o2", "product": "monitor", "qty": 1, "status": "paid"}},
		{"orders", bson.M{"_id": "o3", "product": "mouse", "qty": 3, "status": "shipped"}},
	}
	for _, d := range docs {
		if _, err := w.Put(ctx, d.collection, d.doc); err != nil {
			return err
		}
	}

	proj, err := r.services.Projects.GetProjectByName(project)
	if err != nil {
		return err
	}
	main, err := r.services.Branches.GetBranch(proj.ID, "main")
	if err != nil {
		return err
	}
	_, err = r.services.Pins.Create(proj.ID, main.ID, "baseline", main.HeadLSN,
		"seeded dataset — fork sandboxes from here")
	return err
}

// --- the scripted agent session ---

// demoScenario plays a short two-actor agent session on a fresh branch
// and a conflicting human edit on main — one click, and diff, merge
// conflicts and per-actor undo all have something real to show.
func (r *Router) demoScenario(c *gin.Context) {
	project := r.demoProject(c)
	if project == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized,
			gin.H{"error": "no demo session; POST /api/v1/demo/session first"})
		return
	}
	proj, err := r.services.Projects.GetProjectByName(project)
	if err != nil {
		abortErr(c, http.StatusNotFound, err)
		return
	}
	branches, err := r.services.Branches.ListBranches(proj.ID)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	runs := 0
	for _, b := range branches {
		if strings.HasPrefix(b.Name, "agent-run-") {
			runs++
		}
	}
	name := fmt.Sprintf("agent-run-%d", runs+1)

	main, err := r.services.Branches.GetBranch(proj.ID, "main")
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	if _, err := r.services.Branches.CreateBranch(proj.ID, name, main.ID); err != nil {
		abortErr(c, http.StatusConflict, err)
		return
	}

	ctx := c.Request.Context()
	agent, err := r.services.WriterFor(project, name)
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	// The planner drafts new work…
	agent.SetActor("agent:planner")
	steps := []struct {
		collection string
		doc        bson.M
	}{
		{"orders", bson.M{"_id": "o4", "product": "monitor", "qty": 2, "status": "draft"}},
		{"notes", bson.M{"_id": "plan-1", "text": "restock monitors before the o4 order ships", "order": "o4"}},
	}
	// …and the executor updates existing state, touching o1.
	execSteps := []struct {
		collection string
		doc        bson.M
	}{
		{"orders", bson.M{"_id": "o1", "product": "keyboard", "qty": 2, "status": "refunded"}},
		{"products", bson.M{"_id": "monitor", "name": "Monitor 27q", "price": 329, "stock": 10}},
	}
	for _, s := range steps {
		if _, err := agent.Put(ctx, s.collection, s.doc); err != nil {
			abortErr(c, http.StatusInternalServerError, err)
			return
		}
	}
	agent.SetActor("agent:executor")
	for _, s := range execSteps {
		if _, err := agent.Put(ctx, s.collection, s.doc); err != nil {
			abortErr(c, http.StatusInternalServerError, err)
			return
		}
	}

	// Meanwhile a human touches the same order on main — the merge
	// preview will surface this as a conflict, never silently.
	human, err := r.services.WriterFor(project, "main")
	if err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}
	human.SetActor("user:demo")
	if _, err := human.Put(ctx, "orders",
		bson.M{"_id": "o1", "product": "keyboard", "qty": 2, "status": "expedited"}); err != nil {
		abortErr(c, http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"branch": name,
		"actors": []string{"agent:planner", "agent:executor"},
		"hint":   "diff the branch, preview the merge (one conflict on orders/o1), or undo a single actor",
	})
}

// --- sweeping ---

// startDemoSweeper reclaims expired demo projects once a minute.
func (r *Router) startDemoSweeper() {
	ctx, cancel := context.WithCancel(context.Background())
	r.demo.cancel = cancel
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.sweepDemo()
			}
		}
	}()
}

// sweepDemo runs one reclamation pass: pins deleted, live branches
// released, project (and with it every branch and its history) removed.
func (r *Router) sweepDemo() {
	projects, err := r.services.Projects.ListProjects()
	if err != nil {
		log.Printf("demo sweep: cannot list projects: %v", err)
		return
	}
	cutoff := time.Now().Add(-r.opts.DemoTTL)
	for _, p := range projects {
		if !demoName.MatchString(p.Name) || !p.CreatedAt.Before(cutoff) {
			continue
		}
		if pins, err := r.services.Pins.List(p.ID); err == nil {
			for _, pin := range pins {
				_ = r.services.Pins.Delete(p.ID, pin.Name)
			}
		}
		if branches, err := r.services.Branches.ListBranches(p.ID); err == nil {
			for _, b := range branches {
				r.stopIngester(b.ID)
				if b.IsLive() {
					if err := r.services.Checkout.Release(context.Background(), b.ID); err != nil {
						log.Printf("demo sweep: release %s/%s: %v", p.Name, b.Name, err)
					}
				}
			}
		}
		if err := r.services.Projects.DeleteProject(p.ID); err != nil {
			log.Printf("demo sweep: delete %s: %v", p.Name, err)
			continue
		}
		log.Printf("demo sweep: reclaimed %s", p.Name)
	}
}
