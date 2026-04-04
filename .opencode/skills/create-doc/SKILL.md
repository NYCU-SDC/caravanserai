---
name: create-doc
description: Create or update documentation — implementation notes, tech specs, and PRDs for the Caravanserai project
---

## When to use this skill

Use this when you need to record implementation details, design decisions, research findings, or formal documents (Tech Specs, PRDs) in Notion. Typical triggers:

- You finished implementing a feature and need to document what was built and why
- You made a non-obvious design decision that future agents need to understand
- You need to write a Tech Spec before starting implementation
- You want to capture research findings or discussion notes

## Notion architecture

All teams share two global data sources. Each team page has linked database views filtered by its Product.

| Resource | ID |
|---|---|
| Docs & Specs data source | `collection://2e87dadd-8040-80eb-8a44-000ba07c935d` |
| Meetings & Discussions (Notes) data source | `collection://2e87dadd-8040-8082-9d21-000be6df64de` |
| Caravanserai product page URL | `https://www.notion.so/2e97dadd804080699d0cfb1a8f98c11c` |

## Choosing where to write

| What you're writing | Database | Type property |
|---|---|---|
| Implementation notes, research, design reasoning | Notes | `Note` or `Research` |
| Meeting minutes or discussion records | Notes | `Meeting` or `Discussion` |
| Formal technical design document | Docs & Specs | `Tech Spec` |
| Product requirements document | Docs & Specs | `PRD` |

**Rule of thumb:** If it describes *how* something was built or *why* a decision was made, use Notes. If it defines *what* should be built or *how a system works* as a canonical reference, use Docs & Specs.

## Content language

All Notion content must be written in **Chinese (zh-TW)**. Page titles can include English technical terms where natural (e.g. "Secret Resource Kind 實作筆記").

## Creating a Note

Use `notion_notion-create-pages` with:

```
parent:
  data_source_id: 2e87dadd-8040-8082-9d21-000be6df64de

properties:
  Name: <title in Chinese, English technical terms OK>
  Type: Note            # or Research, Meeting, Discussion
  Tags: Backend Note    # see tag list below
  Status: Active
  Products: https://www.notion.so/2e97dadd804080699d0cfb1a8f98c11c

content: <Notion-flavored markdown in Chinese>
```

### Available Tags for Notes

- `Backend Note` — implementation details, code decisions
- `Backend Planning` — sprint planning, backlog grooming
- `Frontend Note` / `Frontend Planning` — frontend equivalents
- `Business` — product decisions, stakeholder discussions
- `Weekly` / `Monthly` / `Retrospective` / `Office Hours` — meeting types

For Caravanserai backend work, use `Backend Note` as the default tag.

### Note content structure

Write the body in Chinese. Use this structure as a starting point — adapt as needed:

```markdown
## 背景

<Why this note exists. Link to the Jira issue (CARA-N) or Notion doc that prompted the work.>

## 內容

<The main content. For implementation notes: what was built, key design decisions, 
trade-offs considered. For research: findings, comparisons, recommendations.>

## 決策紀錄

<If applicable. Document decisions made and their rationale.>

## 相關連結

- Jira: CARA-<N>
- PR: <link>
- Related docs: <links>
```

## Creating a Doc (Tech Spec or PRD)

Use `notion_notion-create-pages` with:

```
parent:
  data_source_id: 2e87dadd-8040-80eb-8a44-000ba07c935d

properties:
  Name: <title in Chinese>
  Type: Tech Spec       # or PRD
  Status: Draft         # start as Draft, move to Active when reviewed
  Products: https://www.notion.so/2e97dadd804080699d0cfb1a8f98c11c
  Summery: <one-line Chinese summary of the document>

content: <Notion-flavored markdown in Chinese>
```

### Status lifecycle for Docs

`Draft` → `In Review` → `Active` → `Deprecated`

- Create new docs as `Draft`
- Only move to `Active` after review
- Set `Deprecated` when superseded

### Tech Spec content structure

```markdown
## 概述

<One paragraph summary of what this spec covers.>

## 動機

<Why is this needed? What problem does it solve?>

## 設計

<The core design. Include API contracts, data models, component interactions.
Use code blocks for YAML/Go/SQL examples.>

## 替代方案

<What alternatives were considered and why they were rejected.>

## 影響範圍

<What existing components are affected. Migration considerations.>
```

### PRD content structure

```markdown
## 概述

<Product vision and goals.>

## 使用者情境

<Who uses this and how.>

## 功能需求

<Detailed feature requirements.>

## 非功能需求

<Performance, security, reliability constraints.>

## 範圍外

<What is explicitly NOT included.>
```

## Updating an existing page

1. Fetch the page first with `notion_notion-fetch` to get current content
2. Use `notion_notion-update-page` with command `update_content` for targeted edits, or `replace_content` for full rewrites
3. Update `Status` property separately with command `update_properties` if needed

## Linking to a Jira issue from a Note

The Notes database has a `Task` property (free text). Set it to the Jira issue key:

```
properties:
  Task: CARA-<N>
```

## Checklist before creating

1. Correct data source ID for the content type (Notes vs Docs & Specs)
2. `Products` property set to the Caravanserai product URL
3. Content written in Chinese
4. `Status` set appropriately (Active for notes, Draft for new docs)
5. Appropriate `Type` and `Tags` selected
