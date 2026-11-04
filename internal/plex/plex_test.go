package plex

import (
	"encoding/json"
	"testing"

	"github.com/DrSeussFreak/BacklotTracker/internal/model"
)

const webhookEpisode = `{
  "event": "media.scrobble", "user": true, "owner": true,
  "Account": {"id": 1, "title": "alice"},
  "Server": {"title": "srv", "uuid": "abc"},
  "Player": {"local": true, "title": "Living Room", "uuid": "p1"},
  "Metadata": {
    "librarySectionType": "show", "ratingKey": "1234", "key": "/library/metadata/1234",
    "guid": "plex://episode/5d9c", "type": "episode", "title": "Pilot",
    "grandparentTitle": "Some Show", "grandparentRatingKey": "1000",
    "parentIndex": 1, "index": 1, "year": 2008, "duration": 3000000, "viewOffset": 2700000,
    "Guid": [{"id": "imdb://tt0959621"}, {"id": "tmdb://62085"}, {"id": "tvdb://349232"}]
  }
}`

func TestWebhookEpisode(t *testing.T) {
	var p WebhookPayload
	if err := json.Unmarshal([]byte(webhookEpisode), &p); err != nil {
		t.Fatal(err)
	}
	m, ok := ToMedia(p.Metadata)
	if !ok || m.Kind != model.Episode {
		t.Fatalf("kind: %+v", m)
	}
	if m.ShowTitle != "Some Show" || m.Season != 1 || m.Episode != 1 {
		t.Fatalf("episode fields: %+v", m)
	}
	if m.IDs.IMDB != "tt0959621" || m.IDs.TMDB != 62085 || m.IDs.TVDB != 349232 {
		t.Fatalf("ids: %+v", m.IDs)
	}
	if m.PlexGUID != "plex://episode/5d9c" {
		t.Fatalf("guid: %q", m.PlexGUID)
	}
	if got := Progress(&p.Metadata); got != 90 {
		t.Fatalf("progress %v", got)
	}
	if p.Account.Title != "alice" || !p.Owner {
		t.Fatalf("account: %+v", p.Account)
	}
}

func TestLegacyGUID(t *testing.T) {
	m := model.Media{Kind: model.Episode}
	applyGUID("com.plexapp.agents.thetvdb://121361/3/9?lang=en", &m)
	if m.ShowIDs.TVDB != 121361 || m.Season != 3 || m.Episode != 9 {
		t.Fatalf("%+v", m)
	}
	mv := model.Media{Kind: model.Movie}
	applyGUID("com.plexapp.agents.imdb://tt0133093?lang=en", &mv)
	if mv.IDs.IMDB != "tt0133093" {
		t.Fatalf("%+v", mv)
	}
}

func TestSessionUserIDTypes(t *testing.T) {
	for _, s := range []string{`{"User":{"id":"1","title":"a"}}`, `{"User":{"id":1,"title":"a"}}`} {
		var md Metadata
		if err := json.Unmarshal([]byte(s), &md); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if md.User.ID.String() != "1" {
			t.Fatalf("id %q", md.User.ID)
		}
	}
}
