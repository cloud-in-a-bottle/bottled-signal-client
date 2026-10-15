package msgconv

import (
	"html"
	"slices"
	"strings"
	"unicode/utf16"

	"maunium.net/go/mautrix/id"
)

// BodyRange is a mention or a text style over part of a message body, in UTF-16 code units as Signal counts them.
type BodyRange struct {
	Start  int
	Length int
	// BOLD, ITALIC, STRIKETHROUGH, MONOSPACE or SPOILER; or empty for a mention.
	Style      string
	MentionACI string
}

// Person is how a mentioned person is shown.
type Person struct {
	Name string
	// Their user ID on the bridge's Matrix side, for the mention's link.
	MXID id.UserID
}

// mentionPlaceholder is what a Signal body has where a mention goes.
const mentionPlaceholder = "￼"

var styleTags = map[string][2]string{
	"BOLD":          {"<strong>", "</strong>"},
	"ITALIC":        {"<em>", "</em>"},
	"STRIKETHROUGH": {"<del>", "</del>"},
	"MONOSPACE":     {"<code>", "</code>"},
	"SPOILER":       {"<span data-mx-spoiler>", "</span>"},
}

// formatted is a message body with its ranges applied.
type formatted struct {
	// The text, with mentions written out as "@Name".
	Body string
	// HTML with the styles and mention links, or "" when the body has no ranges.
	HTML     string
	Mentions []id.UserID
}

// format applies ranges to body. person looks up a mentioned ACI.
func format(body string, ranges []BodyRange, person func(aci string) Person) formatted {
	if len(ranges) == 0 {
		return formatted{Body: body}
	}
	units := utf16.Encode([]rune(body))
	clamp := func(i int) int { return max(0, min(i, len(units))) }
	cuts := []int{0, len(units)}
	for _, r := range ranges {
		cuts = append(cuts, clamp(r.Start), clamp(r.Start+r.Length))
	}
	slices.Sort(cuts)
	cuts = slices.Compact(cuts)

	var plain, rich strings.Builder
	var mentions []id.UserID
	for i := 0; i+1 < len(cuts); i++ {
		from, to := cuts[i], cuts[i+1]
		text := string(utf16.Decode(units[from:to]))
		var styles []string
		mention := ""
		for _, r := range ranges {
			if r.Start > from || r.Start+r.Length < to {
				continue
			}
			if r.MentionACI != "" {
				mention = r.MentionACI
			} else if _, ok := styleTags[r.Style]; ok && !slices.Contains(styles, r.Style) {
				styles = append(styles, r.Style)
			}
		}
		slices.Sort(styles)
		var segment string
		if mention != "" {
			p := person(mention)
			name := "@" + p.Name
			plain.WriteString(strings.Replace(text, mentionPlaceholder, name, 1))
			segment = `<a href="https://matrix.to/#/` + html.EscapeString(string(p.MXID)) + `">` + html.EscapeString(name) + "</a>"
			mentions = append(mentions, p.MXID)
		} else {
			plain.WriteString(text)
			segment = strings.ReplaceAll(html.EscapeString(text), "\n", "<br>")
		}
		for _, s := range styles {
			rich.WriteString(styleTags[s][0])
		}
		rich.WriteString(segment)
		for _, s := range slices.Backward(styles) {
			rich.WriteString(styleTags[s][1])
		}
	}
	return formatted{Body: plain.String(), HTML: rich.String(), Mentions: mentions}
}
