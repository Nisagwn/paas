package web_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/store"
)

func TestSleepingBadge(t *testing.T) {
	u := setup(t)
	u.login()
	ctx := context.Background()
	u.post("/apps", url.Values{"name": {"blog"}, "repo": {"nisagwn/blog"}, "csrf": {u.csrf("/")}}, "self")
	app, err := u.st.GetAppByName(ctx, "blog")
	if err != nil {
		t.Fatal(err)
	}
	d, _, _ := u.st.EnqueueDeployment(ctx, app.ID, sha(1), "feature", "preview")
	u.st.MarkReady(ctx, d, []store.AliasSpec{{Hostname: "feature-blog.paas.test", Kind: store.AliasPreview, Branch: "feature"}})

	if _, body, _ := u.get("/apps/blog"); strings.Contains(body, ">Uykuda<") {
		t.Fatal("awake deployment shown as sleeping")
	}
	if err := u.st.SetSleeping(ctx, d.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/apps/blog", fmt.Sprintf("/deployments/%d", d.ID)} {
		if code, body, _ := u.get(path); code != 200 || !strings.Contains(body, ">Uykuda<") {
			t.Fatalf("%s: %d, no sleeping badge\n%s", path, code, body)
		}
	}
}
