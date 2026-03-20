package telemost

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// DeriveDisplayNames generates deterministic display names from a shared token.
// Both client and server independently compute the same name pairs.
// Returns serverNames[0..n-1] and clientNames[0..n-1].
// Names look like natural Telemost participant names (Russian names, nicknames).
func DeriveDisplayNames(token string, n int) (serverNames, clientNames []string) {
	used := make(map[string]bool)
	serverNames = make([]string, n)
	clientNames = make([]string, n)

	for i := 0; i < n; i++ {
		serverNames[i] = deriveUniqueName(token, "s", i, used)
	}
	for i := 0; i < n; i++ {
		clientNames[i] = deriveUniqueName(token, "c", i, used)
	}
	return
}

// deriveUniqueName generates a deterministic name and retries with incrementing
// suffix if the name collides with an already-used one.
func deriveUniqueName(token, role string, index int, used map[string]bool) string {
	for attempt := 0; attempt < 100; attempt++ {
		name := deriveName(token, role, index+attempt*1000)
		if !used[name] {
			used[name] = true
			return name
		}
	}
	// Fallback: should never happen with reasonable name space.
	name := deriveName(token, role, index)
	used[name] = true
	return name
}

// deriveName produces a single deterministic display name from HMAC(token, role+index).
func deriveName(token, role string, index int) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(fmt.Sprintf("%s-%d", role, index)))
	h := mac.Sum(nil) // 32 bytes

	// Use first 8 bytes as deterministic seed.
	seed := binary.BigEndian.Uint64(h[:8])
	r := rand.New(rand.NewPCG(seed, binary.BigEndian.Uint64(h[8:16])))

	male := r.IntN(2) == 0
	switch r.IntN(4) {
	case 0:
		// First name only.
		return seededFirstName(r, male)
	case 1:
		// Last name only.
		return seededLastName(r, male)
	case 2:
		// First + Last or Last + First.
		first := seededFirstName(r, male)
		last := seededLastName(r, male)
		if r.IntN(2) == 0 {
			return first + " " + last
		}
		return last + " " + first
	default:
		// Latin nickname.
		return seededNickname(r)
	}
}

func seededFirstName(r *rand.Rand, male bool) string {
	if male {
		return provider.MaleFirstNames[r.IntN(len(provider.MaleFirstNames))]
	}
	return provider.FemaleFirstNames[r.IntN(len(provider.FemaleFirstNames))]
}

func seededLastName(r *rand.Rand, male bool) string {
	pair := provider.LastNames[r.IntN(len(provider.LastNames))]
	if male {
		return pair.Male
	}
	return pair.Female
}

func seededNickname(r *rand.Rand) string {
	switch r.IntN(3) {
	case 0:
		pair := provider.LastNames[r.IntN(len(provider.LastNames))]
		base := strings.ToLower(provider.Transliterate(pair.Male))
		return fmt.Sprintf("%s%d", base, r.IntN(100))
	case 1:
		return provider.NickPrefixes[r.IntN(len(provider.NickPrefixes))] + provider.NickBases[r.IntN(len(provider.NickBases))]
	default:
		return fmt.Sprintf("%s%d", provider.NickBases[r.IntN(len(provider.NickBases))], r.IntN(1000))
	}
}
