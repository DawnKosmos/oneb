package oneb_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DawnKosmos/oneb"
	"github.com/DawnKosmos/oneb/oneb_gen"
)

func TestAuthFrontendGenerationWithoutRuntime(t *testing.T) {
	dir := t.TempDir()
	if err := oneb_gen.Generate((*oneb.ChatGPTClient)(nil).Routes, dir); err != nil {
		t.Fatal(err)
	}
	models, err := os.ReadFile(filepath.Join(dir, "models.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access_token", "refresh_token", "credentials"} {
		if strings.Contains(string(models), secret) {
			t.Fatal("frontend contracts expose credentials")
		}
	}
	client, err := os.ReadFile(filepath.Join(dir, "client.ts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"auth/start", "auth/status", "auth/logout"} {
		if !strings.Contains(string(client), method) {
			t.Fatal("missing generated auth method")
		}
	}
}
