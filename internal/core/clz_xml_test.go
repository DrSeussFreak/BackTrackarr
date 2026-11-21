package core

import (
	"context"
	"strings"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
)

func TestImportCLZXML(t *testing.T) {
	e, st := shelfEngine(t, config.ShelfConfig{User: "me"})
	ctx := context.Background()
	doc := "\xef\xbb\xbf" + `<?xml version="1.0" encoding="ISO-8859-1"?>
<comicinfo><comiclist>
 <comic><id>77</id><mainsection><series><displayname>Am` + "\xe9" + `lie</displayname></series><title>Chapter One</title></mainsection>
  <issue>1</issue><publisher><displayname>Image</displayname></publisher>
  <creditlist><credit><person><displayname>Not The Series</displayname></person></credit></creditlist>
  <releasedate><date>2012-03-14</date></releasedate><readdate><date>2024-02-03</date></readdate><myrating>9</myrating><extrafield>kept</extrafield></comic>
 <comic><id>78</id><series>Saga</series><issue>2</issue><readit>Yes</readit></comic>
</comiclist></comicinfo>`
	res, err := e.ImportCLZ(ctx, strings.NewReader(doc), "clz:test.xml")
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 2 || res.New != 2 || res.Reads != 1 {
		t.Fatalf("%+v", res)
	}
	var series, title, pub, raw string
	var rating float64
	if err := st.DB.QueryRow(`SELECT series,title,publisher,rating,raw FROM comics WHERE ext_key='clz:77'`).Scan(&series, &title, &pub, &rating, &raw); err != nil {
		t.Fatal(err)
	}
	if series != "Amélie" || title != "Chapter One" || pub != "Image" || rating != 9 || !strings.Contains(raw, "extrafield") {
		t.Fatalf("got %q %q %q %v", series, title, pub, rating)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM comics WHERE ext_key='clz:78' AND is_read=1`); n != 1 {
		t.Error("read flag not imported")
	}
	again, err := e.ImportCLZ(ctx, strings.NewReader(doc), "clz:test.xml")
	if err != nil || again.New != 0 || again.Reads != 0 {
		t.Fatalf("re-import: %+v %v", again, err)
	}
	if _, err := e.ImportCLZ(ctx, strings.NewReader(`<root><other/></root>`), "x"); err == nil {
		t.Error("XML with no comics should say so")
	}
	p := uniFile(t, "clz.xml", doc)
	u, err := e.ImportAnything(ctx, p, "clz.xml", "", nil)
	if err != nil || u.Comics != 2 {
		t.Fatalf("universal import: %+v %v", u, err)
	}
}
