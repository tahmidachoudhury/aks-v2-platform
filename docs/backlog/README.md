# Project Backlog

**TL;DR:** `notion-backlog.csv` holds every work item for the platform: 116 cards in 13 phases,
all set to `Backlog`. Import it into Notion as a database, convert `Status` to a Status property
with four options, then add a Board view grouped by Status.

## Columns

| Column | Notion property type | Values |
|---|---|---|
| Name | Title | Task name |
| ID | Text | `P3-07` style; referenced by Depends On |
| Phase | Select | `P00 Foundations and Decisions` to `P12 Teardown` |
| Status | Status | Backlog, In progress, Blocked, Completed |
| Priority | Select | Must, Should, Stretch |
| Type | Select | Decision, Build, Config, Test, Docs |
| Area | Multi-select | Comma-separated tags (Terraform, Networking, Secrets, ...) |
| Depends On | Text | Comma-separated IDs of cards that should finish first |
| Description | Text | What to do and why |
| Acceptance Criteria | Text | How you know it is done |

Phases carry a two-digit prefix so they sort in order. The Decision cards in P00 pick the event
bus, manifest tooling and secrets delivery. Later cards assume Service Bus, Kustomize and External
Secrets Operator, so if a decision goes the other way, reword the dependent cards.

## Import into Notion

1. In Notion, go to **Import** (sidebar, or Settings) and pick **CSV**. Upload `notion-backlog.csv`.
2. At the mapping step, set Name as the title and map the property types from the table above.
   Map Status as Select for now.
3. Open the new database, click the **Status** column header, **Edit property**, and change the
   type to **Status**. Put `Backlog` in the To-do group, `In progress` and `Blocked` in the
   In progress group, and `Completed` in the Complete group. Add any of the four options that
   do not exist yet.
4. Add a view: **Board**, grouped by Status. Filter or sub-group by Phase to work one phase at a
   time.

Alternative: create an empty database with the Status property already set up as above, then use
**... > Merge with CSV**. Merge only adds rows and never updates existing ones, so re-importing
creates duplicates.

Notion imports can't create relations. If you want Depends On as a real relation, add a relation
property to the same database after import and link the cards by hand, using the text column as
the guide.
