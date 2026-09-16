# Helpers for manual end-to-end tests against a Fizzy development server.
# The development server returns the magic-link code in a response header,
# so these helpers can log in as any user without email.
#
#   . script/e2e-lib.sh
#   export FIZZY_URL=http://app.fizzy.localhost:3006 ACCOUNT=338000007
#   login david@example.com > david.session
#   export SESSION=$(cat david.session) BOARD=<board id> BOT_NAME=Claude
#   card=$(mkcard "Title" "<p>Description</p>")
#   mention $card "What is 6 times 7?"
#   comments $card

: "${FIZZY_URL:=http://app.fizzy.localhost:3006}"

login() { # email -> session token
  local headers code pending
  headers=$(curl -s -D - -o /dev/null -X POST -H "Content-Type: application/json" -H "Accept: application/json" \
    -d "{\"email_address\":\"$1\"}" "$FIZZY_URL/session" | tr -d '\r')
  code=$(echo "$headers" | sed -n 's/^x-magic-link-code: //p')
  pending=$(echo "$headers" | sed -n 's/^set-cookie: pending_authentication_token=\([^;]*\);.*/\1/p')
  curl -s -X POST -H "Content-Type: application/json" -H "Accept: application/json" \
    -H "Cookie: pending_authentication_token=$pending" -d "{\"code\":\"$code\"}" \
    "$FIZZY_URL/session/magic_link" | jq -r .session_token
}

api() { # curl arguments, with a path as the last argument
  local args=("$@") last=$(($# - 1))
  args[last]="$FIZZY_URL/$ACCOUNT${args[last]}"
  curl -s -H "Accept: application/json" -H "Content-Type: application/json" -H "Cookie: session_token=$SESSION" "${args[@]}"
}

bot_mention_tag() {
  local sgid
  sgid=$(curl -s -H "Cookie: session_token=$SESSION" "$FIZZY_URL/$ACCOUNT/prompts/users" |
    grep -o "<lexxy-prompt-item search=\"$BOT_NAME[^>]*>" | sed -n 's/.*sgid="\([^"]*\)".*/\1/p' | head -1)
  echo "<action-text-attachment sgid=\"$sgid\" content-type=\"application/vnd.actiontext.mention\"></action-text-attachment>"
}

mkcard() { # title, description html -> card number
  api -X POST -D - -o /dev/null -d "$(jq -n --arg t "$1" --arg d "$2" '{card:{title:$t,description:$d}}')" "/boards/$BOARD/cards" |
    tr -d '\r' | sed -n 's/^location: .*\/cards\/\([0-9]*\).*/\1/p'
}

mention() { # card number, text
  api -X POST -o /dev/null -w "mention on #$1: %{http_code}\n" \
    -d "$(jq -n --arg b "<p>$(bot_mention_tag) $2</p>" '{comment:{body:$b}}')" "/cards/$1/comments"
}

comments() { # card number
  api "/cards/$1/comments" | jq -r '.[] | "[\(.created_at[11:19])] \(.creator.name): \(.body.plain_text)\n"'
}
