package api

import (
	"net/http"
	"testing"
)

func TestHTTPStatusForRecoveringIsConflict(t *testing.T) {
	if got := httpStatusFor(CodeRecovering); got != http.StatusConflict {
		t.Fatalf("httpStatusFor(recovering)=%d want 409", got)
	}
}
