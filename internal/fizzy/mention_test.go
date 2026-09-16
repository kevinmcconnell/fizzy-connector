package fizzy

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func sgid(envelope string) string {
	return base64.StdEncoding.EncodeToString([]byte(envelope)) + "--0123456789abcdef"
}

func mentionTag(sgid string) string {
	return fmt.Sprintf(`<action-text-attachment sgid="%s" content-type="application/vnd.actiontext.mention"><img src="/a.png"> Claude</action-text-attachment>`, sgid)
}

func TestMentionedUserIDs(t *testing.T) {
	direct := sgid(`{"_rails":{"data":"gid://fizzy/User/03f5abc?expires_in","pur":"attachable"}}`)
	nested := sgid(fmt.Sprintf(`{"_rails":{"message":"%s","pur":"attachable"}}`,
		base64.StdEncoding.EncodeToString([]byte(`"gid://fizzy/User/03f5xyz?expires_in"`))))
	upload := fmt.Sprintf(`<action-text-attachment sgid="%s" content-type="image/png"></action-text-attachment>`,
		sgid(`{"_rails":{"data":"gid://fizzy/ActiveStorage::Blob/9"}}`))

	html := "<p>" + mentionTag(direct) + " and " + mentionTag(nested) + upload + " @plain text</p>"

	assert.Equal(t, []string{"03f5abc", "03f5xyz"}, MentionedUserIDs(html))
	assert.True(t, Mentions(html, "03f5xyz"))
	assert.False(t, Mentions(html, "9"), "an upload is not a mention")
	assert.False(t, Mentions("<p>@Claude</p>", "03f5abc"), "plain text is not a mention")
}

func TestPlainText(t *testing.T) {
	html := `<p>See <a href="https://fizzy.example/1/cards/42">the other card</a><br>and <a href="https://x.example">https://x.example</a></p><ul><li>one &amp; two</li></ul>`

	assert.Equal(t, "See the other card (https://fizzy.example/1/cards/42)\nand https://x.example\n- one & two", PlainText(html))
}
