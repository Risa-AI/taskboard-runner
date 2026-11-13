package apitest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Ported from the original suite's TestExpectBody: every operator and the nested-object
// recursion, against one representative payload.
func TestAssertBody_Operators(t *testing.T) {
	body := []byte(`{
		"success": true,
		"data": {
			"token": "jwt-abc.123",
			"user": {
				"id": "usr_42",
				"email": "e2e-abc@example.com",
				"shops": [{"id": "shop_1"}, {"id": "shop_2"}]
			},
			"count": 2
		}
	}`)
	r := NewRunner("")
	r.Seed("rand", "abc")

	for _, tc := range []struct {
		name string
		spec map[string]interface{}
		ok   bool
	}{
		{"primitive equality with substitution", map[string]interface{}{"success": true, "data.user.email": "e2e-${rand}@example.com"}, true},
		{"primitive mismatch", map[string]interface{}{"success": false}, false},
		{"yaml int equals json float", map[string]interface{}{"data.count": 2}, true},
		{"$exists true", map[string]interface{}{"data.token": map[string]interface{}{"$exists": true}}, true},
		{"$exists false on missing path", map[string]interface{}{"data.missing": map[string]interface{}{"$exists": false}}, true},
		{"$type string", map[string]interface{}{"data.token": map[string]interface{}{"$type": "string"}}, true},
		{"$type number", map[string]interface{}{"data.count": map[string]interface{}{"$type": "number"}}, true},
		{"$len on array", map[string]interface{}{"data.user.shops": map[string]interface{}{"$len": 2}}, true},
		{"$len mismatch", map[string]interface{}{"data.user.shops": map[string]interface{}{"$len": 5}}, false},
		{"$regex match", map[string]interface{}{"data.token": map[string]interface{}{"$regex": "^jwt-[a-z0-9.]+$"}}, true},
		{"$regex no match", map[string]interface{}{"data.token": map[string]interface{}{"$regex": "^bearer-"}}, false},
		{"$contains substring", map[string]interface{}{"data.user.email": map[string]interface{}{"$contains": "@example.com"}}, true},
		{"$contains array element", map[string]interface{}{"data.user.shops": map[string]interface{}{"$contains": map[string]interface{}{"id": "shop_1"}}}, true},
		{"array index path", map[string]interface{}{"data.user.shops.1.id": "shop_2"}, true},
		{"plain nested object recursion", map[string]interface{}{"data.user": map[string]interface{}{"id": "usr_42", "email": "e2e-${rand}@example.com"}}, true},
		{"combined operators", map[string]interface{}{"data.token": map[string]interface{}{"$exists": true, "$type": "string", "$regex": "^jwt-"}}, true},
		{"unknown operator", map[string]interface{}{"data.token": map[string]interface{}{"$bogus": 1}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := r.assertBody(tc.spec, body)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// With two wrong fields the same one is reported every time. The original runner walked
// the spec in Go's random map order, so the message about one response could
// change between two runs -- and the same input must give the same result.
func TestAssertBody_ReportsTheSameFailureEveryTime(t *testing.T) {
	r := NewRunner("")
	body := []byte(`{"a": 1, "b": 2, "c": 3}`)
	spec := map[string]interface{}{"c": 30, "a": 10, "b": 20}
	for i := 0; i < 20; i++ {
		err := r.assertBody(spec, body)
		require.EqualError(t, err, "a: expected 10, got 1")
	}
}

// The failure message quotes strings and names the full path, the shape
// the original suite's README documents, so it can be pasted back into the YAML.
func TestAssertBody_MessagesNameThePath(t *testing.T) {
	r := NewRunner("")
	err := r.assertBody(map[string]interface{}{"data.user.email": "e2e-xyz@example.com"}, []byte(`{"data":{"user":{"email":"e2e-abc@example.com"}}}`))
	require.EqualError(t, err, `data.user.email: expected "e2e-xyz@example.com", got "e2e-abc@example.com"`)

	err = r.assertBody(map[string]interface{}{"data": map[string]interface{}{"user": map[string]interface{}{"role": "support"}}}, []byte(`{"data":{"user":{}}}`))
	require.EqualError(t, err, `data.user.role: expected "support", got <nil>`)

	err = r.assertBody(map[string]interface{}{"a": 1}, []byte(`not json`))
	require.ErrorContains(t, err, "the response is not JSON")
}
