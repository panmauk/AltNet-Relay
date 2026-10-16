package mailbox

import (
	"errors"
	"testing"
)

func TestVulgarAndHatefulNamesAreRefused(t *testing.T) {
	bad := []string{
		// English, plain and dressed up
		"fuck", "fuckyou", "fuuuck", "f_u_c_k", "f-u-c-k", "shit", "sh1t", "5hit", "bullshit", "shit_head",
		"big_ass", "a55", "ass2000", "ass_hole", "asshole", "dumb-bitch", "b1tch", "whoreson", "slut99", "s1ut",
		"nigger", "n1gger", "nigg4", "faggot", "f4ggot", "retard", "tranny", "wanker", "pussy", "p0rn",
		"pornking", "cumshot", "big_cock", "cock_sucker", "d1ldo", "hitler", "hitler88", "nazi", "nazi_gang",
		"sex_bot", "xxx_videos", "kys", "rape_you", "pedo_bear", "cunt", "c0ck", "d1ck", "big_dick", "titty_twister",
		"heil_hitler", "sieg_heil", "white_power", "x_x_x",
		// Czech and Slovak
		"kurva", "kokot", "cur4k", "hovno", "zmrd", "jebat", "pica_ceska", "debil", "prdel",
		// German, French, Spanish, Italian, Polish, Russian
		"scheisse", "arschloch", "hurensohn", "mierda", "puta", "putain", "salope", "cazzo", "kurwa",
		"pierdol", "blyat", "pidor", "suka_pro", "mudak",
	}
	for _, name := range bad {
		if err := CheckHandleDecency(name); !errors.Is(err, ErrIndecentName) {
			t.Errorf("%q should be refused, got %v", name, err)
		}
	}
}

// The reason a name filter is hard: innocent words that contain bad ones.
func TestInnocentNamesAreNotCaught(t *testing.T) {
	good := []string{
		"rob", "kate", "alice", "test1", "test2", "panmox", "gabriel", "jan_novak", "petr-svoboda", "anna88",
		"classic", "classy_dev", "assassin", "bassist", "passport", "massive", "assume", "grape", "grapes",
		"therapist", "therapy", "analyst", "analog", "canal", "scunthorpe_fan", "scunthorpe", "dickens", "dickinson",
		"cockburn", "cocktail", "peacock", "hancock", "sussex", "essex", "penistone", "shiitake", "titan", "titanic",
		"attitude", "button", "butter", "cummings", "document", "cucumber", "circumstance", "arsenal", "parsley",
		"pedometer", "torpedo", "computer", "nazir", "nazim", "heilbronn", "spicy", "cocoon", "raccoon", "pakistan",
		"skype", "niggle", "snigger", "wankel", "slutsky", "picasso", "pictures", "debilitated", "sukanya",
		"mermaid", "putative", "compute", "repute", "dispute", "joderlin", "troian", "merdad", "sextant",
		"gaymer", "hell_yeah", "damn_good", "crazy_frog", "big_boss", "l33t", "h3ll0", "b0ss", "j4ne",
		"user_123", "a1b2c3", "p_a_s_s", "n_o_t",
	}
	for _, name := range good {
		if err := CheckHandleDecency(name); err != nil {
			t.Errorf("%q should be fine, got %v", name, err)
		}
	}
}

func TestDecencyCheckIsCaseBlindAndQuiet(t *testing.T) {
	if CheckHandleDecency("FuCk") == nil || CheckHandleDecency("@fuck") == nil {
		t.Fatal("capitals and a leading @ must not get round the check")
	}
	// The message must not point at the word, or it teaches how to avoid it.
	err := CheckHandleDecency("shithead")
	if err == nil || len(err.Error()) > 80 {
		t.Fatalf("a short, generic refusal: %v", err)
	}
}
