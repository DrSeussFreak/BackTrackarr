package core

import (
	"context"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func audibleItem(t *testing.T, st *store.Store, key string) (string, string, float64, float64, int64) {
	t.Helper()
	var title, status string
	var rating, progress float64
	var dur int64
	err := st.DB.QueryRow(`SELECT title, COALESCE(status,''), COALESCE(rating,0), COALESCE(progress,0), COALESCE(duration_s,0)
		FROM shelf_items WHERE media_type='audiobook' AND source='audible' AND ext_key=?`, key).Scan(&title, &status, &rating, &progress, &dur)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return title, status, rating, progress, dur
}

func audibleCount(t *testing.T, st *store.Store, key, kind string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM shelf_events e JOIN shelf_items i ON i.id=e.item_id WHERE i.ext_key=? AND e.kind=?`, key, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAudibleLibraryAndListening(t *testing.T) {
	eng, st, _ := testEngine(t, newFakePlex("srvA"), nil)
	ctx := context.Background()
	lib := "Added,Title,Series,Authors,Narrators,Length,Progress,Release Date,My Rating,Rating,ASIN\n" +
		"1,Project Hail Mary,,Andy Weir,Ray Porter,16 hrs and 10 mins,Finished,2021-05-04,5,4.8,B08G9PRS1K\n" +
		"2,Dune,Dune (book 1),Frank Herbert,Simon Vance,20 hrs,10h 0m left,2007-01-01,,4.5,B002V1OF70\n" +
		"3,\"=HYPERLINK(\"\"https://audible.com/pd/x\"\",\"\"Sheets Title\"\")\",,A,B,1 hr and 28 mins,0,2020-01-01,,3.9,B000000003\n" +
		"4,Almost Done,,A,B,2 hrs,&lt;1 min left,2020-01-01,,4,B000000004\n"
	for i := 0; i < 2; i++ {
		res, err := eng.ImportBooks(ctx, strings.NewReader(lib), "")
		if err != nil || res.Source != "audible" || res.Rows != 4 {
			t.Fatalf("library import: %+v %v", res, err)
		}
	}
	title, status, rating, progress, dur := audibleItem(t, st, "asin:B08G9PRS1K")
	if title != "Project Hail Mary" || status != "read" || rating != 5 || progress != 1 || dur != 16*3600+600 {
		t.Fatalf("finished title: %q %q %v %v %d", title, status, rating, progress, dur)
	}
	if n := audibleCount(t, st, "asin:B08G9PRS1K", "finish"); n != 1 {
		t.Fatalf("re-import must keep one finish, got %d", n)
	}
	if _, status, rating, progress, _ := audibleItem(t, st, "asin:B002V1OF70"); status != "reading" || rating != 0 || progress != .5 {
		t.Fatalf("public rating or progress wrong: %q %v %v", status, rating, progress)
	}
	if title, status, _, _, _ := audibleItem(t, st, "asin:B000000003"); title != "Sheets Title" || status != "to-read" {
		t.Fatalf("sheets row: %q %q", title, status)
	}
	if _, status, _, progress, _ := audibleItem(t, st, "asin:B000000004"); status != "read" || progress < .99 {
		t.Fatalf("<1 min left is past the 90%% setting: %q %v", status, progress)
	}

	// Starting a re-listen must not erase the earlier finish.
	relisten := "Title,Authors,Narrators,Length,Progress,ASIN\nProject Hail Mary,Andy Weir,Ray Porter,16 hrs and 10 mins,8h 5m left,B08G9PRS1K\n"
	if _, err := eng.ImportBooks(ctx, strings.NewReader(relisten), ""); err != nil {
		t.Fatal(err)
	}
	if _, status, _, _, _ := audibleItem(t, st, "asin:B08G9PRS1K"); status != "reading" || audibleCount(t, st, "asin:B08G9PRS1K", "finish") != 1 {
		t.Fatalf("re-listen: status %q finishes %d", status, audibleCount(t, st, "asin:B08G9PRS1K", "finish"))
	}

	// Listening sessions: separate rows, no duplicates, title preserved.
	listen := "Start Date,ASIN,Event Duration Milliseconds\n2026-03-01 20:00:00,B08G9PRS1K,1800000\n2026-03-02 20:00:00,B08G9PRS1K,3600000\n"
	for i := 0; i < 2; i++ {
		if _, err := eng.ImportBooks(ctx, strings.NewReader(listen), ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := audibleCount(t, st, "asin:B08G9PRS1K", "session"); n != 2 {
		t.Fatalf("sessions: %d", n)
	}
	if title, _, _, _, _ := audibleItem(t, st, "asin:B08G9PRS1K"); title != "Project Hail Mary" {
		t.Fatalf("listening row renamed the title: %q", title)
	}
	var secs int64
	st.DB.QueryRow(`SELECT SUM(duration_s) FROM shelf_events WHERE kind='session'`).Scan(&secs)
	if secs != 5400 {
		t.Fatalf("listened seconds: %d", secs)
	}
}

func TestAudibleExtractorGoodreadsMode(t *testing.T) {
	eng, st, _ := testEngine(t, newFakePlex("srvA"), nil)
	file := "Title,Author,Publisher,My Rating,Average Rating,Binding,Year Published,Date Added,Bookshelves,ISBN,ISBN13,ASIN\n" +
		"Heat,A Writer,Pub,4,4.2,Audiobook,2019,2026-11-20,read audible audiobook fiction,,,B000000009\n"
	res, err := eng.ImportBooks(context.Background(), strings.NewReader(file), "")
	if err != nil || res.Source != "audible" {
		t.Fatalf("%+v %v", res, err)
	}
	_, status, rating, _, _ := audibleItem(t, st, "asin:B000000009")
	if status != "read" || rating != 4 {
		t.Fatalf("%q %v", status, rating)
	}
	var at int64
	st.DB.QueryRow(`SELECT MAX(at) FROM shelf_events`).Scan(&at)
	if at != 0 {
		t.Fatal("the extension's Date Added is the export date and must not become a listen date")
	}
}

func TestAudibleDetection(t *testing.T) {
	h := func(s string) []string { r, _ := csv.NewReader(strings.NewReader(s)).Read(); return r }
	cases := []struct {
		header, name, want string
	}{
		{"Title,Authors,Narrators,Progress,ASIN", "", "library"},
		{"Book Id,Title,Author,Exclusive Shelf,Bookshelves,ASIN,Year Published,Average Rating", "", ""},
		{"Title,Date,Duration,ASIN", "Digital.PrimeVideo.Viewinghistory.csv", ""},
		{"Product Name,ASIN,Start Date,Event Duration Milliseconds", "Audible.Listening/Listening.csv", "listening"},
		{"Product Name,ASIN,Order Date", "Audible.PurchaseHistory/Orders.csv", ""},
		{"Product Name,ASIN", "Audible.Library/Library.csv", "library"},
	}
	for _, c := range cases {
		if got := audibleKind(h(c.header), c.name); got != c.want {
			t.Errorf("%q in %q: got %q want %q", c.header, c.name, got, c.want)
		}
	}
}
