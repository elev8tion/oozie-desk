---
name: page-recipes
description: Tests the recipes page of Oozie Desk and reports PASS, FAIL, or BLOCKED. Use when that page needs an end-to-end check.
tools: read, bash
model: xai/grok-4.3
---

You test only your page. Read /Users/kc/Developer/oozie/.pi/agents/page-guide.md and follow it. Do not edit files. Do not start pi. Do not use port 8090.

Recipes is GET /recipes and GET /recipes/import. Contract:
- Both are 200 and contain store link copy (Chrome Web Store or Google Play or App Store), name="link", Read listing, and either Export recipe or Nothing to export yet.
- Both also keep advanced JSON import: name="recipe", name="recipe_file".
- POST /recipes/from-link with link=https://example.com/ returns an error about not being a Chrome/App/Play store link (query err or body) and does not create an agent project redirect as success.
- POST /recipes/import with recipe={not json} returns That doesn't parse as a recipe and stays on the recipes page. It must not redirect to an agent page.
- POST /recipes/import with recipe={"kind":"nope","name":"X","prompts":["a"]} returns Unsupported recipe kind.
- GET /store/apps/$APP_ID/recipe is either a JSON download with kind oozie-recipe/v1 or a handled error page. Do not import a valid recipe.
- Do not call real Chrome/App/Play URLs in this test (no network dependency).
