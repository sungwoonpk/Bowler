package signature_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/sungwoonpk/bowler/signature"
)

// A receiver checks every webhook before it trusts it. Read the body exactly as it arrived, before parsing it, because
// the signature covers those bytes.
func ExampleVerify() {
	secret := "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw" // given to you when the endpoint was created

	receiver := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "cannot read the body", http.StatusBadRequest)
			return
		}
		err = signature.Verify(
			[]string{secret}, // pass the old secret too while one is being rotated
			r.Header.Get(signature.HeaderID),
			r.Header.Get(signature.HeaderTimestamp),
			r.Header.Get(signature.HeaderSignature),
			body, time.Now(), 0, // 0 means the default tolerance of five minutes
		)
		if err != nil {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}
		// The webhook is genuine. Ignore a webhook-id you have already handled, then do the work.
		w.WriteHeader(http.StatusOK)
	})

	// What Bowler sends, built here so the example can run.
	send := func(body string) int {
		now := time.Now()
		sig, _ := signature.Sign(secret, "msg_2LJe8vvBgjS4G7tRyLKw1N9bXx1", now, []byte(`{"invoice":"in_1"}`))
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBufferString(body))
		req.Header.Set(signature.HeaderID, "msg_2LJe8vvBgjS4G7tRyLKw1N9bXx1")
		req.Header.Set(signature.HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
		req.Header.Set(signature.HeaderSignature, sig)
		rec := httptest.NewRecorder()
		receiver.ServeHTTP(rec, req)
		return rec.Code
	}

	fmt.Println("genuine:", send(`{"invoice":"in_1"}`))
	fmt.Println("changed on the way:", send(`{"invoice":"in_2"}`))
	// Output:
	// genuine: 200
	// changed on the way: 400
}

func ExampleGenerateSecret() {
	secret, _ := signature.GenerateSecret()
	fmt.Println(len(secret), secret[:6])
	// Output: 38 whsec_
}
