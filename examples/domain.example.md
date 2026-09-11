# Domain rules for the QA / data-analyst agent (example)

Put a file like this at the path named by `agent.domain_file` in vigil.yaml. It is injected into every
Browser Agent task (bounded to 12 KB) as "Domain rules". Give each rule an id so findings can cite it.

## Identity and structure (ID-*)
- ID-1 An *order* belongs to exactly one *customer*; the order list shows one row per order.
- ID-2 A *product* may appear in several *categories*; duplicated names in a category list are expected.

## Display vs data consistency (DATA-*)
- DATA-1 Every count in a card or badge must equal the length of the list the same screen loads from its API.
- DATA-2 Totals must equal the sum of their rows (cart total vs. line totals, incl. rounding rules: 2 decimals, half-up).
- DATA-3 Prices are stored in cents; the UI divides by 100. A value 100× too large is a unit bug.
- DATA-4 Dates are shown in the user's timezone; a date one day off from the API value (UTC midnight) is a timezone bug.
- DATA-5 "No data" is correct only when the API list is empty; a non-empty list with an empty-state message is `display`.

## Behavioural rules (RULE-*)
- RULE-1 A discontinued product still appears in past orders. Do not report it as stale data.
- RULE-2 Checkout is disabled while the cart is empty; an enabled button with an empty cart is a bug.

## Mutation policy
- Production is read-only: never submit, delete, send, or pay.
- On staging, reversible actions are allowed with the named test accounts only.
- Never type into a field labelled password; never leave the allowed hosts.

## What to write in `findings`
`kind` ∈ data_mismatch | domain_rule | display | accessibility. `where` = URL + element text.
`expected`/`actual` = both values with their sources (API path + json path, DOM selector).
`evidence` = one line: request URL, response snippet, screenshot name. Cite the rule id when one applies.
