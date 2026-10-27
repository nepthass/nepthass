package entities_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
)

func TestNormalizeAlias(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantDisplay string
		wantKey     string
		wantErr     bool
	}{
		{"plain", "Living Room", "Living Room", "living room", false},
		{"trims and collapses whitespace", "  living \t  room \n", "living room", "living room", false},
		{"punctuation allowed", "Kitchen's (main) lamp - 1.", "Kitchen's (main) lamp - 1.", "kitchen's (main) lamp - 1.", false},
		{"arabic", "\u063a\u0631\u0641\u0629 \u0627\u0644\u0645\u0639\u064a\u0634\u0629", "\u063a\u0631\u0641\u0629 \u0627\u0644\u0645\u0639\u064a\u0634\u0629", "\u063a\u0631\u0641\u0629 \u0627\u0644\u0645\u0639\u064a\u0634\u0629", false},
		{"accents", "L\u00e1mpara sala", "L\u00e1mpara sala", "l\u00e1mpara sala", false},
		{"max length", strings.Repeat("a", entities.MaxAliasRunes), strings.Repeat("a", entities.MaxAliasRunes), strings.Repeat("a", entities.MaxAliasRunes), false},
		{"empty", "", "", "", true},
		{"only spaces", "   ", "", "", true},
		{"too long", strings.Repeat("a", entities.MaxAliasRunes+1), "", "", true},
		{"markdown", "*bold*", "", "", true},
		{"underscore", "a_b", "", "", true},
		{"mention", "@everyone", "", "", true},
		{"user mention syntax", "<@123456789>", "", "", true},
		{"channel hash", "#general", "", "", true},
		{"backtick", "`code`", "", "", true},
		{"zero-width space", "Living\u200bRoom", "", "", true},
		{"emoji", "\U0001F4A1 lamp", "", "", true},
		{"looks like an entity id", "light.kitchen", "", "", true},
		{"looks like an entity id in upper case", "LIGHT.Kitchen", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			display, key, err := entities.NormalizeAlias(tt.in)

			if tt.wantErr {
				if !errors.Is(err, entities.ErrInvalidAlias) {
					t.Fatalf("NormalizeAlias(%q) error = %v, want ErrInvalidAlias", tt.in, err)
				}
				return
			}
			if err != nil || display != tt.wantDisplay || key != tt.wantKey {
				t.Errorf("NormalizeAlias(%q) = %q, %q, %v", tt.in, display, key, err)
			}
		})
	}
}

func set(t *testing.T, f fixture, id, alias string) error {
	t.Helper()
	return f.aliases.Set(context.Background(), entities.AliasSet{ID: entities.ID(id), Alias: alias, By: admin.UserID, At: now})
}

func TestAliases_setGetRemoveAndUpdate(t *testing.T) {
	f := newFixture(t)

	if err := set(t, f, "light.a", "Living Room"); err != nil {
		t.Fatal(err)
	}
	got, _ := f.aliases.Get(context.Background(), "light.a")
	if err := set(t, f, "light.a", "Lounge"); err != nil {
		t.Fatal(err)
	}
	updated, _ := f.aliases.Get(context.Background(), "light.a")
	reused := set(t, f, "light.b", "Living Room") // the old alias was released by the update
	removed, removeErr := f.aliases.Remove(context.Background(), "light.a")
	again, _ := f.aliases.Remove(context.Background(), "light.a")
	none, _ := f.aliases.Get(context.Background(), "light.a")

	if got != "Living Room" || updated != "Lounge" || reused != nil || !removed || removeErr != nil || again || none != "" {
		t.Errorf("got=%q updated=%q reused=%v removed=%v/%v again=%v none=%q", got, updated, reused, removed, removeErr, again, none)
	}
}

func TestAliases_duplicatesAreRejectedDeterministically_caseInsensitively(t *testing.T) {
	f := newFixture(t)
	if err := set(t, f, "light.a", "Living Room"); err != nil {
		t.Fatal(err)
	}

	err := set(t, f, "light.b", "  living   ROOM ")
	same := set(t, f, "light.a", "living room") // re-saving one's own alias is fine

	var taken *entities.AliasTakenError
	if !errors.Is(err, entities.ErrAliasTaken) || !errors.As(err, &taken) || taken.Holder != "light.a" {
		t.Errorf("duplicate error = %v, want AliasTakenError naming light.a", err)
	}
	if same != nil {
		t.Errorf("re-saving the same alias on the same entity failed: %v", same)
	}
	if alias, _ := f.aliases.Get(context.Background(), "light.b"); alias != "" {
		t.Errorf("a rejected alias was stored: %q", alias)
	}
}

func TestResolve_acceptsIDs_andAliasesOnlyForEntitiesTheCallerMayUse(t *testing.T) {
	f := newFixture(t, "light.kitchen", "sensor.hall")
	f.grant(t, "light.kitchen", entities.AccessControl)
	f.grant(t, "sensor.hall", entities.AccessRead)
	for id, alias := range map[string]string{"light.kitchen": "Kitchen", "sensor.hall": "Hall Sensor", "light.secret": "Secret Lamp"} {
		if err := set(t, f, id, alias); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	resolve := func(actor auth.Actor, in string, min entities.Access) (entities.ID, error) {
		return f.svc.Resolve(ctx, actor, in, min)
	}

	byID, errID := resolve(user, "light.kitchen", entities.AccessControl)
	byAlias, errAlias := resolve(user, "  kitchen ", entities.AccessControl)
	readOnlyForControl, errReadOnly := resolve(user, "Hall Sensor", entities.AccessControl)
	readOnlyForRead, errRead := resolve(user, "hall sensor", entities.AccessRead)
	_, errUnshared := resolve(user, "Secret Lamp", entities.AccessRead)
	_, errUnknown := resolve(user, "Does Not Exist", entities.AccessRead)
	_, errMarkdown := resolve(user, "*kitchen*", entities.AccessRead)
	_, errLevel := resolve(auth.Actor{UserID: "1"}, "kitchen", entities.AccessRead)

	if errID != nil || byID != "light.kitchen" || errAlias != nil || byAlias != "light.kitchen" {
		t.Errorf("id=%q/%v alias=%q/%v", byID, errID, byAlias, errAlias)
	}
	if readOnlyForRead != "sensor.hall" || errRead != nil {
		t.Errorf("read-only alias for read = %q, %v", readOnlyForRead, errRead)
	}
	for name, err := range map[string]error{"read-only alias used for control": errReadOnly, "alias of an unshared entity": errUnshared, "unknown alias": errUnknown, "invalid alias": errMarkdown} {
		if !errors.Is(err, entities.ErrInvalidID) {
			t.Errorf("%s: error = %v, want ErrInvalidID (indistinguishable from an unknown name)", name, err)
		}
	}
	if readOnlyForControl != "" || !errors.Is(errLevel, auth.ErrNotAuthorized) {
		t.Errorf("readOnlyForControl=%q errLevel=%v", readOnlyForControl, errLevel)
	}
}

func TestServices_showAliases_onlyForSharedEntities_andSuggestByAlias(t *testing.T) {
	f := newFixture(t, "light.kitchen", "light.private")
	f.grant(t, "light.kitchen", entities.AccessControl)
	if err := set(t, f, "light.kitchen", "Kitchen Main"); err != nil {
		t.Fatal(err)
	}
	if err := set(t, f, "light.private", "Private Lamp"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	st, statusErr := f.svc.Status(ctx, user, "light.kitchen")
	_, hidden := f.svc.Status(ctx, user, "light.private")
	page, devErr := f.svc.Devices(ctx, user, 1)
	byAlias, suggestErr := f.svc.Suggest(ctx, user, entities.AccessRead, "main")
	byPrivate, _ := f.svc.Suggest(ctx, user, entities.AccessRead, "private")

	if statusErr != nil || st.Alias != "Kitchen Main" || st.DisplayName() != "Kitchen Main" {
		t.Errorf("Status alias = %q (%v)", st.Alias, statusErr)
	}
	if !errors.Is(hidden, entities.ErrAccessDenied) {
		t.Errorf("unshared entity error = %v", hidden)
	}
	if devErr != nil || len(page.Devices) != 1 || page.Devices[0].Alias != "Kitchen Main" {
		t.Errorf("Devices = %+v (%v)", page, devErr)
	}
	if suggestErr != nil || len(byAlias) != 1 || byAlias[0].ID != "light.kitchen" || byAlias[0].Alias != "Kitchen Main" {
		t.Errorf("Suggest by alias = %+v (%v)", byAlias, suggestErr)
	}
	if len(byPrivate) != 0 {
		t.Errorf("an unshared entity's alias leaked through autocomplete: %+v", byPrivate)
	}
}
