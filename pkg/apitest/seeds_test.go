package apitest

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

func fixedRand() RandSource { return rand.New(rand.NewPCG(1, 2)) }

func loadExampleSeeds(t *testing.T) Seeds {
	t.Helper()
	data, err := os.ReadFile("testdata/example_seeds.yaml")
	require.NoError(t, err)
	var s Seeds
	require.NoError(t, yaml.Unmarshal(data, &s))
	return s
}

// exampleSeeds is the original suite's e2e_test.go seeding, copied as it is, so the
// data form can be checked against the code it replaces. Its random parts
// (rand, phoneSuffix) are left out and checked for shape instead.
func exampleSeeds(now time.Time) map[string]string {
	shopClock := time.FixedZone("WIB", 7*60*60)
	n := now.In(shopClock)
	booking := n.Add(2 * time.Hour)
	voucherSlot := time.Date(n.Year(), n.Month(), n.Day(), 10, 0, 0, 0, shopClock).AddDate(0, 0, 3)
	loyaltyDay := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, shopClock).AddDate(0, 0, 5)
	return map[string]string{
		"dateNow":          n.Format("2006-01-02"),
		"bookingDate":      booking.Format("2006-01-02"),
		"bookingTime":      booking.Format("15:04"),
		"dateTomorrow":     n.AddDate(0, 0, 1).Format("2006-01-02"),
		"voucherSlotDate":  voucherSlot.Format("2006-01-02"),
		"voucherSlotTime":  voucherSlot.Format("15:04"),
		"voucherSlotDow":   strconv.Itoa(int(voucherSlot.Weekday())),
		"loyaltyDate":      loyaltyDay.Format("2006-01-02"),
		"loyaltyTime1":     "09:00",
		"loyaltyTime2":     "11:00",
		"loyaltyTime3":     "13:00",
		"loyaltyTime4":     "15:00",
		"loyaltyTime5":     "17:00",
		"loyaltyTime6":     "16:00",
		"voucherNowDow":    strconv.Itoa(int(n.Weekday())),
		"voucherStartDate": n.AddDate(0, 0, -1).Format("2006-01-02"),
		"voucherEndDate":   n.AddDate(0, 0, 30).Format("2006-01-02"),
	}
}

// The original suite's seeds, written as data, give what its Go code gives -- at
// several times of day, including 23:00 WIB, where +2h crosses midnight and
// 00:30, where the UTC date is still yesterday. TGT-2's acceptance: if the
// data form cannot say what e2e_test.go computes, the original suite cannot
// run unchanged.
func TestSeeds_ReproduceTheGoSeeding(t *testing.T) {
	spec := loadExampleSeeds(t)
	wib := time.FixedZone("WIB", 7*60*60)
	for _, now := range []time.Time{
		time.Date(2026, 9, 30, 23, 0, 0, 0, wib),
		time.Date(2026, 10, 1, 0, 30, 0, 0, wib),
		time.Date(2026, 12, 31, 12, 0, 0, 0, wib),
	} {
		t.Run(now.Format(time.RFC3339), func(t *testing.T) {
			got, err := spec.Resolve(now.UTC(), fixedRand())
			require.NoError(t, err)
			for k, want := range exampleSeeds(now) {
				require.Equal(t, want, got.Vars[k], k)
			}
			require.Equal(t, strconv.FormatInt(now.UnixNano(), 36), got.Vars["rand"])
			require.Len(t, got.Vars["phoneSuffix"], 8)
			require.Equal(t, "0812"+got.Vars["phoneSuffix"], got.Vars["ownerPhone"])
			require.Equal(t, "+62812"+got.Vars["phoneSuffix"], got.Vars["ownerPhoneE164"])
			require.Contains(t, got.Numbers, "voucherSlotDow", "a weekday a server decodes into []int must stay a number")
			require.Contains(t, got.Numbers, "voucherNowDow")
		})
	}
}

// The booking pair: at 23:00 both halves land on the next day together. The
// bug this prevents was a date from `now` and a time from `now+2h` -- a
// booking 22 hours in the past, refused by the future-only guard.
func TestSeeds_ADateAndATimeFromOneInstantMoveTogether(t *testing.T) {
	spec := Seeds{
		"now":         {Time: "Asia/Jakarta"},
		"booking":     {From: "now", Offset: "2h"},
		"bookingDate": {From: "booking", Format: "2006-01-02"},
		"bookingTime": {From: "booking", Format: "15:04"},
	}
	at := time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC) // 23:00 in Jakarta
	got, err := spec.Resolve(at, fixedRand())
	require.NoError(t, err)
	require.Equal(t, "2026-10-01", got.Vars["bookingDate"])
	require.Equal(t, "01:00", got.Vars["bookingTime"])
}

// Each refusal names the seed and says what to write instead: a person fixes
// a target's seeds from this sentence alone.
func TestSeeds_Refusals(t *testing.T) {
	one := "x"
	for _, tc := range []struct {
		name string
		spec Seeds
		want string
	}{
		{"no source", Seeds{"a": {}}, "seed a: give exactly one of value, random, digits, template, time or from (it has 0)"},
		{"two sources", Seeds{"a": {Value: &one, Digits: 3}}, "(it has 2)"},
		{"cycle", Seeds{"a": {From: "b"}, "b": {From: "a"}}, "a cycle: a -> b -> a"},
		{"from nothing", Seeds{"a": {From: "missing"}}, `seed a: from "missing", which is not a seed`},
		{"from a non-time", Seeds{"a": {From: "b"}, "b": {Value: &one}}, `from "b", which is not a time seed`},
		{"template unknown", Seeds{"a": {Template: "x${nope}"}}, "uses ${nope}, which is not a seed"},
		{"bad zone", Seeds{"a": {Time: "Mars/Olympus"}}, "is not a time zone"},
		{"bad at", Seeds{"a": {Time: "UTC", At: "25:99"}}, "write a clock time such as 10:00"},
		{"bad offset", Seeds{"a": {Time: "UTC", Offset: "two hours"}}, "write a duration such as 2h"},
		{"shaping a non-time", Seeds{"a": {Value: &one, Days: 1}}, "shape a time seed"},
		{"weekday and format", Seeds{"a": {Time: "UTC", Weekday: true, Format: "Mon"}}, "give one"},
		{"number that is not", Seeds{"a": {Value: &one, Number: true}}, `number is set but "x" is not a number`},
		{"bad random kind", Seeds{"a": {Random: "hex"}}, "the one kind is base36"},
		{"bad name", Seeds{"1a": {Value: &one}}, "a variable name is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.spec.Resolve(time.Unix(0, 0), fixedRand())
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// With several broken seeds the same one is named every time.
func TestSeeds_TheSameSpecGivesTheSameRefusal(t *testing.T) {
	spec := Seeds{"z": {}, "b": {}, "m": {}}
	for i := 0; i < 20; i++ {
		_, err := spec.Resolve(time.Unix(0, 0), fixedRand())
		require.ErrorContains(t, err, "seed b:")
	}
}

// Resolved seeds land in a runner the way the original suite's Seed and SeedNumber
// calls did: a number variable is a number in a body and text in a path.
func TestResolved_ApplySeedsARunner(t *testing.T) {
	five := "5"
	got, err := Seeds{"dow": {Value: &five, Number: true}, "name": {Template: "day-${dow}"}}.Resolve(time.Unix(0, 0), fixedRand())
	require.NoError(t, err)
	r := NewRunner("")
	got.Apply(r)
	require.Equal(t, "day-5", r.Vars["name"])
	require.Equal(t, float64(5), r.substituteAny("${dow}"))
	require.Equal(t, "/x/5", r.substitute("/x/${dow}"))
	require.Equal(t, fmt.Sprint([]string{"dow", "name"}), fmt.Sprint(Seeds{"name": {}, "dow": {}}.Names()))
}
