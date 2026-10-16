package mailbox

import (
	"errors"
	"regexp"
	"strings"
)

// Decency of @names.
//
// A name is the one thing about a person that everyone who types it sees, so
// vulgar, hateful and sexual names are refused when they are claimed. The
// check is on the name only (never on messages, which nobody but the two
// people can read) and it is a filter, not a judge: it stops the obvious, and
// reports and bans deal with the rest.
//
// What it does, and why it is built this way:
//
//   - A name is split at "-" and "_" into words. Long, unmistakable words
//     (strongWords) are refused anywhere inside a word, and may be stretched
//     (fuuuck). Short words that live inside innocent ones -- "ass" in
//     "classic", "cum" in "document", "rape" in "grape" -- are refused only as
//     a whole word (shortWords), so "big_ass" is out and "classic" is in.
//   - Digits that stand in for letters are read as letters (sh1t, a55, n1gg4)
//     and digits stuck on the end are ignored (ass2000). A run of single
//     letters is joined (f_u_c_k).
//   - A few real words that contain a refused one are allowed by name
//     (scunthorpe, niggle).
//
// It is not perfect, and doesn't pretend to be: odd separators (fu_ck) and
// words in languages not listed get through. The lists below are a start, in
// plain sight so they can be extended; they cover English and the languages
// most Relay users are likely to write -- Czech and Slovak, German, French,
// Spanish, Italian, Polish and transliterated Russian.

// ErrIndecentName is returned for a name that isn't allowed. It never says
// which part was the problem: that would be a guide to getting round it.
var ErrIndecentName = errors.New("that name isn't allowed here; please choose another")

// CheckHandleDecency reports ErrIndecentName for a vulgar or hateful @name.
// The character rules are ValidHandle's job, not this one's.
func CheckHandleDecency(handle string) error {
	if indecent(CanonicalHandle(handle)) {
		return ErrIndecentName
	}
	return nil
}

func indecent(name string) bool {
	tokens := splitName(name)
	// Runs of single letters read as one word: f_u_c_k.
	candidates := append([]string(nil), tokens...)
	run := ""
	for _, t := range append(append([]string(nil), tokens...), "") {
		if len(t) == 1 {
			run += t
			continue
		}
		if len(run) >= 3 {
			candidates = append(candidates, run)
		}
		run = ""
	}
	for _, tok := range candidates {
		if allowedWords[tok] {
			continue
		}
		for _, form := range readings(tok) {
			if shortSet[form] || matchStrong(form) {
				return true
			}
		}
	}
	// A few phrases are refused even when spread over several words.
	squashed := strings.NewReplacer("-", "", "_", "").Replace(name)
	for _, p := range phrases {
		if strings.Contains(squashed, p) {
			return true
		}
	}
	return false
}

// splitName breaks a name at the separators a handle can have.
func splitName(name string) []string {
	return strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
}

// readings are the ways a word can be read: as written, with digits that look
// like letters turned into them (1 as i or as l), and with digits on the
// ends dropped.
func readings(tok string) []string {
	forms := []string{tok}
	add := func(s string) {
		for _, f := range forms {
			if f == s {
				return
			}
		}
		if s != "" {
			forms = append(forms, s)
		}
	}
	add(strings.Trim(tok, "0123456789"))
	for _, one := range []string{"i", "l"} {
		leet := strings.NewReplacer("0", "o", "1", one, "3", "e", "4", "a", "5", "s", "7", "t", "8", "b").Replace(tok)
		add(leet)
		add(strings.Trim(leet, "0123456789"))
	}
	return forms
}

func matchStrong(s string) bool {
	for _, re := range strongRes {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// stretchy turns a word into a pattern that allows each letter to repeat:
// "fuck" matches fuck, fuuck, ffuucckk.
func stretchy(w string) *regexp.Regexp {
	var b strings.Builder
	for _, r := range w {
		b.WriteRune(r)
		b.WriteByte('+')
	}
	return regexp.MustCompile(b.String())
}

var (
	strongRes []*regexp.Regexp
	shortSet  = map[string]bool{}
	// allowedWords are real words that contain one of the refused ones.
	allowedWords = map[string]bool{}
)

func init() {
	for _, w := range strongWords {
		strongRes = append(strongRes, stretchy(w))
	}
	for _, w := range shortWords {
		shortSet[w] = true
	}
	for _, w := range allowed {
		allowedWords[w] = true
	}
}

// strongWords are refused anywhere inside a word of the name.
var strongWords = []string{
	// English: profanity, sexual, slurs
	"fuck", "shit", "cunt", "bitch", "whore", "slut", "nigg", "faggot", "retard", "tranny",
	"wanker", "wanking", "bollock", "pussy", "cumshot", "blowjob", "handjob", "rimjob", "dildo", "jizz",
	"porn", "hentai", "gangbang", "incest", "asshole", "arsehole", "dickhead", "motherfuck", "cocksuck",
	"pedophile", "paedophile", "masturbat", "orgasm", "vagina", "penis", "hitler",
	// Czech and Slovak
	"kurva", "kurev", "curak", "kokot", "hovno", "hovna", "zmrd", "mrdat", "mrdka", "jebat", "jebem",
	"jebnut", "pojeb", "buzna", "buzerant", "prdel", "cigan",
	// German
	"scheiss", "scheisz", "arschloch", "hurensohn", "fotze", "wichser", "schlampe", "missgeburt",
	// French
	"merde", "putain", "connard", "connasse", "salope", "encule",
	// Spanish and Italian
	"mierda", "cabron", "pendejo", "chingada", "chingar", "maricon", "gilipollas",
	"cazzo", "vaffanculo", "stronzo", "puttana",
	// Polish and Russian (transliterated)
	"kurwa", "pierdol", "jebac", "chuj", "skurwysyn", "blyat", "blyad", "pizd", "pidor", "pidar",
	"mudak", "gandon", "zhopa", "yebat", "yeban",
}

// shortWords are refused only when they are a whole word of the name: inside
// longer words they are everywhere (classic, document, grape, torpedo).
var shortWords = []string{
	// English
	"ass", "arse", "anus", "anal", "cock", "cocks", "cum", "tit", "tits", "titty", "dick", "dicks",
	"fag", "fags", "fuk", "fck", "fcuk", "twat", "wank", "sex", "sexy", "xxx", "milf", "boob", "boobs", "boobies",
	"rape", "rapes", "raped", "raper", "rapist", "pedo", "paedo", "nazi", "nazis", "heil", "kys",
	"kike", "spic", "chink", "gook", "paki", "coon", "negro", "neger",
	// Czech and Slovak
	"pica", "pico", "picus", "cuza", "cuna", "srat", "debil", "debile", "debilni",
	// German, French, Spanish, Italian, Russian
	"hure", "ficken", "ficker", "fick", "spast", "spasti", "pute", "nique", "niquer",
	"puta", "puto", "joder", "troia", "merda", "suka",
}

// phrases are refused when they appear once the separators are taken out.
var phrases = []string{"siegheil", "heilhitler", "whitepower", "gasthejews", "killallniggers"}

// allowed are whole words, and places and surnames, that would otherwise be
// caught by a refused word inside them.
var allowed = []string{
	"scunthorpe", "niggle", "niggling", "niggardly", "snigger", "sniggering",
	"slutsk", "slutsky", "slutskiy", "jizzakh", "penistone", "wankel", "wankdorf",
	"shiitake", "shiitakes",
}
