package value

import (
	"encoding/json"
	"testing"
)

// FuzzJSONRoundTrip checks that any JSON document plait can decode encodes
// back to a value that decodes identically.
func FuzzJSONRoundTrip(f *testing.F) {
	for _, s := range []string{`null`, `{"a":[1,2.5,"x",true,null]}`, `{"__plait_unknown__":true}`, `[[],{}]`, `"é"`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		var v Value
		if err := json.Unmarshal([]byte(doc), &v); err != nil {
			return
		}
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", v.GoString(), err)
		}
		var back Value
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("re-decode %s: %v", data, err)
		}
		if v.IsWhollyKnown() && !v.Equal(back) {
			t.Fatalf("round trip changed %s into %s", v.GoString(), back.GoString())
		}
	})
}
