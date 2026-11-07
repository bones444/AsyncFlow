package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Gurkunwar/asyncflow/internal/models"
	"github.com/Gurkunwar/asyncflow/internal/services"
	"github.com/bwmarrin/discordgo"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Discord is mocked with an in-memory RoundTripper. No network, no real token.

type fakeGuild struct {
	ownerID string
	roles   []discordgo.Role
	members map[string][]string // userID -> role IDs
}

type fakeDiscord struct {
	guilds map[string]fakeGuild
}

func (f *fakeDiscord) RoundTrip(req *http.Request) (*http.Response, error) {
	body, status := f.serve(req)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (f *fakeDiscord) serve(req *http.Request) (string, int) {
	const notFound = `{"message":"Unknown","code":10004}`

	// Discord messages (poll creation sends one).
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/messages") {
		return `{"id":"msg1","channel_id":"chan1"}`, http.StatusOK
	}

	i := strings.Index(req.URL.Path, "/guilds/")
	if i < 0 {
		return notFound, http.StatusNotFound
	}
	parts := strings.Split(strings.Trim(req.URL.Path[i+len("/guilds/"):], "/"), "/")
	g, ok := f.guilds[parts[0]]
	if !ok {
		return notFound, http.StatusNotFound
	}

	switch {
	case len(parts) == 1: // GET guild
		b, _ := json.Marshal(map[string]any{"id": parts[0], "owner_id": g.ownerID, "roles": g.roles})
		return string(b), http.StatusOK
	case len(parts) == 2 && parts[1] == "roles":
		b, _ := json.Marshal(g.roles)
		return string(b), http.StatusOK
	case len(parts) == 2 && parts[1] == "channels":
		return `[{"id":"chan1","name":"general","type":0}]`, http.StatusOK
	case len(parts) == 2 && parts[1] == "members":
		return `[{"user":{"id":"u-admin"},"roles":[]}]`, http.StatusOK
	case len(parts) == 3 && parts[1] == "members":
		roles, ok := g.members[parts[2]]
		if !ok {
			return `{"message":"Unknown Member","code":10007}`, http.StatusNotFound
		}
		b, _ := json.Marshal(map[string]any{"user": map[string]string{"id": parts[2]}, "roles": roles})
		return string(b), http.StatusOK
	}
	return notFound, http.StatusNotFound
}

const (
	gMain     = "1000" // guild with ordinary @everyone
	gEveryone = "2000" // guild where @everyone has Administrator
	uOwner    = "u-owner"
	uAdmin    = "u-admin"
	uMgr      = "u-mgr"      // has Manage Server through a role
	uPlain    = "u-plain"    // ordinary role, no admin bits
	uNoRole   = "u-norole"   // member with no roles besides @everyone
	uOutsider = "u-outsider" // not in the guild at all
	uStdMgr   = "u-stdmgr"   // manages a standup, but is not an admin
	rAdmin    = "r-admin"
	rMgr      = "r-mgr"
	rBasic    = "r-basic"
)

func testFakeDiscord() *fakeDiscord {
	return &fakeDiscord{guilds: map[string]fakeGuild{
		gMain: {
			ownerID: uOwner,
			roles: []discordgo.Role{
				{ID: gMain, Name: "@everyone", Permissions: discordgo.PermissionViewChannel},
				{ID: rAdmin, Name: "admin", Permissions: discordgo.PermissionAdministrator},
				{ID: rMgr, Name: "mgr", Permissions: discordgo.PermissionManageGuild},
				{ID: rBasic, Name: "basic", Permissions: discordgo.PermissionSendMessages},
			},
			members: map[string][]string{
				uAdmin:  {rAdmin},
				uMgr:    {rMgr},
				uPlain:  {rBasic},
				uNoRole: {},
				uStdMgr: {rBasic},
			},
		},
		gEveryone: {
			ownerID: uOwner,
			roles: []discordgo.Role{
				{ID: gEveryone, Name: "@everyone", Permissions: discordgo.PermissionAdministrator},
			},
			members: map[string][]string{uNoRole: {}},
		},
	}}
}

// testDB connects to a disposable Postgres given by TEST_DATABASE_URL.
// Skips when it is not set. The schema is reset on every call.
func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB-backed tests")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public;").Error; err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := db.AutoMigrate(&models.Guild{}, &models.UserProfile{}, &models.StandupHistory{},
		&models.Standup{}, &models.Poll{}, &models.PollOption{}, &models.PollVote{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newTestServer(t *testing.T, db *gorm.DB) *Server {
	t.Helper()
	dg, err := discordgo.New("Bot test-token-not-real")
	if err != nil {
		t.Fatal(err)
	}
	dg.Client = &http.Client{Transport: testFakeDiscord()}
	return &Server{
		DB:             db,
		Session:        dg,
		StandupService: &services.StandupService{DB: db, Session: dg},
		PollService:    services.NewPollService(db, dg),
	}
}

func TestIsGuildAdminAndCanManageStandup(t *testing.T) {
	s := newTestServer(t, nil)

	isAdmin := []struct {
		name, user, guild string
		want              bool
	}{
		{"owner", uOwner, gMain, true},
		{"admin via role", uAdmin, gMain, true},
		{"manage server via role", uMgr, gMain, true},
		{"ordinary member", uPlain, gMain, false},
		{"member without roles", uNoRole, gMain, false},
		{"not in guild", uOutsider, gMain, false},
		{"empty user id", "", gMain, false},
		{"@everyone grants admin", uNoRole, gEveryone, true},
	}
	for _, c := range isAdmin {
		t.Run("isGuildAdmin/"+c.name, func(t *testing.T) {
			if got := s.isGuildAdmin(c.user, c.guild); got != c.want {
				t.Fatalf("isGuildAdmin(%q,%q)=%v, want %v", c.user, c.guild, got, c.want)
			}
		})
	}

	standup := models.Standup{GuildID: gMain, ManagerID: uStdMgr}
	canManage := []struct {
		name, user string
		want       bool
	}{
		{"standup manager (not admin)", uStdMgr, true},
		{"guild admin (not manager)", uAdmin, true},
		{"guild owner", uOwner, true},
		{"ordinary member", uPlain, false},
		{"member without roles", uNoRole, false},
		{"outsider", uOutsider, false},
		{"empty user id", "", false},
	}
	for _, c := range canManage {
		t.Run("canManageStandup/"+c.name, func(t *testing.T) {
			if got := s.canManageStandup(c.user, standup); got != c.want {
				t.Fatalf("canManageStandup(%q)=%v, want %v", c.user, got, c.want)
			}
		})
	}
}

// call runs a handler directly with the user already set in context
// (the JWT layer is outside this test).
func call(h http.HandlerFunc, method, target, userID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), UserIDKey, userID))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestPermissionEndpoints(t *testing.T) {
	db := testDB(t)
	s := newTestServer(t, db)

	// Standup owned by uStdMgr in gMain. Standup endpoints check the manager or an admin.
	if err := db.Create(&models.Guild{GuildID: gMain}).Error; err != nil {
		t.Fatal(err)
	}
	standup := models.Standup{Name: "daily", GuildID: gMain, ManagerID: uStdMgr, Time: "09:00", Days: "mon"}
	if err := db.Create(&standup).Error; err != nil {
		t.Fatal(err)
	}
	sid := strconv.FormatUint(uint64(standup.ID), 10)

	type ep struct {
		name, method, target, body string
		handler                    http.HandlerFunc
		managerOK                  bool // standup manager (not admin) allowed
		adminCode                  int  // expected status for owner/admin
	}
	endpoints := []ep{
		{"standups/history", http.MethodGet, "/api/standups/history?standup_id=" + sid, "", s.HandleGetStandupHistory, true, 200},
		{"standups/get", http.MethodGet, "/api/standups/get?id=" + sid, "", s.HandleGetStandup, true, 200},
		{"standups/add-member", http.MethodPost, "/api/standups/add-member",
			`{"standup_id":` + sid + `,"user_id":"u-new"}`, s.HandleAddStandupMember, true, 200},
		{"standups/remove-member", http.MethodPost, "/api/standups/remove-member",
			`{"standup_id":` + sid + `,"user_id":"u-new"}`, s.HandleRemoveStandupMember, true, 200},
		{"standups/create", http.MethodPost, "/api/standups/create",
			`{"name":"n","time":"09:00","days":"mon","guild_id":"` + gMain + `","report_channel_id":"chan1","questions":["q"]}`,
			s.HandleCreateStandup, false,
			// The test bot is not connected to Discord, so the handler stops at
			// verifyBotPermissions with 500 after the authz check has passed.
			http.StatusInternalServerError},
		{"polls/create", http.MethodPost, "/api/polls/create",
			`{"guild_id":"` + gMain + `","channel_id":"chan1","question":"q","duration":60,"options":["a","b"]}`,
			s.HandleCreateWebPoll, false, http.StatusCreated},
		{"guild-channels", http.MethodGet, "/api/guild-channels?guild_id=" + gMain, "", s.HandleGetGuildChannels, false, 200},
		{"guild-members", http.MethodGet, "/api/guild-members?guild_id=" + gMain, "", s.HandleGetGuildMembers, false, 200},
		{"guilds/roles", http.MethodGet, "/api/guilds/roles?guild_id=" + gMain, "", s.HandleGetGuildRoles, false, 200},
	}

	for _, e := range endpoints {
		// Ordinary members, members without roles and outsiders: 403.
		for _, user := range []string{uPlain, uNoRole, uOutsider} {
			t.Run(e.name+"/403/"+user, func(t *testing.T) {
				rec := call(e.handler, e.method, e.target, user, e.body)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status=%d body=%q, want 403", rec.Code, rec.Body.String())
				}
			})
		}

		// Guild owner and admin (via role): expected success code.
		for _, user := range []string{uOwner, uAdmin} {
			t.Run(e.name+"/admitted/"+user, func(t *testing.T) {
				rec := call(e.handler, e.method, e.target, user, e.body)
				if rec.Code != e.adminCode {
					t.Fatalf("status=%d body=%q, want %d", rec.Code, rec.Body.String(), e.adminCode)
				}
			})
		}

		// Standup manager who is not an admin: 200 only where the manager rule applies.
		want := http.StatusForbidden
		if e.managerOK {
			want = http.StatusOK
		}
		t.Run(e.name+"/standup-manager", func(t *testing.T) {
			rec := call(e.handler, e.method, e.target, uStdMgr, e.body)
			if rec.Code != want {
				t.Fatalf("status=%d body=%q, want %d", rec.Code, rec.Body.String(), want)
			}
		})
	}
}
