package replay

// Verdict is how a rule change moves one round's decision.
type Verdict string

const (
	Same    Verdict = "same"    // no change
	Closer  Verdict = "closer"  // changed, and now nearer to what the owner actually did
	Farther Verdict = "farther" // changed, and now further from it
	Changed Verdict = "changed" // changed, with nothing to judge it against (or a tie)
	Unknown Verdict = "unknown" // a replay could not be read
)

// sameText is how similar two replies must be to count as the same reply.
const sameText = 0.6

// Compare judges the decision under the new rules (after) against the old (before) and the owner's.
func Compare(before, after, actual Decision) Verdict {
	if !before.Valid || !after.Valid {
		return Unknown
	}
	if before.Action == after.Action && (before.Action != ActionReply || Similarity(before.Text, after.Text) >= sameText) {
		return Same
	}
	if !actual.Valid {
		return Changed
	}
	b, a := score(before, actual), score(after, actual)
	switch {
	case a > b:
		return Closer
	case a < b:
		return Farther
	}
	return Changed
}

// score is how near a decision is to the owner's: the action first, then the reply's wording.
func score(d, actual Decision) float64 {
	if d.Action != actual.Action {
		return 0
	}
	if d.Action != ActionReply {
		return 2
	}
	return 1 + Similarity(d.Text, actual.Text)
}

// Similarity is the mean of the character and character-pair Dice coefficients of two texts (0..1).
// Characters alone credit a paraphrase that keeps the words; pairs keep word order in the picture.
func Similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	return (dice(grams(ra, 1), grams(rb, 1)) + dice(grams(ra, 2), grams(rb, 2))) / 2
}

func grams(r []rune, n int) map[string]int {
	out := map[string]int{}
	for i := 0; i+n <= len(r); i++ {
		out[string(r[i:i+n])]++
	}
	return out
}

func dice(x, y map[string]int) float64 {
	total, common := 0, 0
	for g, n := range x {
		total += n
		common += min(n, y[g])
	}
	for _, n := range y {
		total += n
	}
	if total == 0 {
		return 0
	}
	return 2 * float64(common) / float64(total)
}
