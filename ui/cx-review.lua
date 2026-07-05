-- cx-review.lua — in-neovim review UI for cx doc edits.
-- Written by cx to ~/.local/share/cx/cx-review.lua on editor launch; do not edit.
--
-- Proposed text is inserted as REAL buffer lines (green) below the lines it
-- replaces (red), so it scrolls, searches, and edits like normal text. Every
-- hunk lands in the quickfix list (]q / [q to jump). With the cursor on a
-- hunk: y keeps the green (applies), n keeps the red (skips), N also asks
-- why and tells cx immediately, a applies all, u undoes the last decision,
-- q finishes. Decisions land in edits-done.json for cx.
--
-- The buffer temporarily holds both versions during review; the file on disk
-- is only written when the review finishes. Extmarks track the blocks, so
-- your own edits during the review don't break anything.

local ns = vim.api.nvim_create_namespace("cx_review")
local datadir = vim.fn.expand("~/.local/share/cx")

local S = { hunks = nil, buf = nil, total = 0, hist = {} }

local function lines_of(s)
  return vim.split(s, "\n", { plain = true })
end

local function locate(buf, search_lines)
  local total = vim.api.nvim_buf_line_count(buf)
  local n = #search_lines
  local function try(norm)
    for i = 0, total - n do
      local chunk = vim.api.nvim_buf_get_lines(buf, i, i + n, false)
      local match = true
      for j = 1, n do
        local a, b = chunk[j], search_lines[j]
        if norm then
          a, b = a:gsub("%s+$", ""), b:gsub("%s+$", "")
        end
        if a ~= b then
          match = false
          break
        end
      end
      if match then
        return i
      end
    end
    return nil
  end
  return try(false) or try(true)
end

local function common_affixes(a, b)
  local p = 0
  local maxp = math.min(#a, #b)
  while p < maxp and a[p + 1] == b[p + 1] do
    p = p + 1
  end
  local sfx = 0
  while sfx < math.min(#a, #b) - p and a[#a - sfx] == b[#b - sfx] do
    sfx = sfx + 1
  end
  return p, sfx
end

-- split_diff finds the changed byte span between two single ASCII lines,
-- snapped to word boundaries.
local function split_diff(old, new)
  local p = 0
  local maxp = math.min(#old, #new)
  while p < maxp and old:byte(p + 1) == new:byte(p + 1) do
    p = p + 1
  end
  local oe, ne = #old, #new
  while oe > p and ne > p and old:byte(oe) == new:byte(ne) do
    oe = oe - 1
    ne = ne - 1
  end
  while p > 0 and old:sub(p, p) ~= " " do
    p = p - 1
  end
  while oe < #old and ne < #new and old:sub(oe + 1, oe + 1) ~= " " do
    oe = oe + 1
    ne = ne + 1
  end
  return p, oe, ne
end

local function is_ascii(s)
  return not s:find("[\128-\255]")
end

-- mark_range returns the current {startRow, endRowExclusive} of an extmark.
local function mark_range(id)
  if not id then
    return nil
  end
  local m = vim.api.nvim_buf_get_extmark_by_id(S.buf, ns, id, { details = true })
  if not m or #m == 0 then
    return nil
  end
  local endRow = (m[3] and m[3].end_row) or m[1]
  if endRow <= m[1] then
    endRow = m[1] + 1 -- single-line (word-diff) marks
  end
  return { m[1], endRow }
end

local function del_marks(h)
  for _, k in ipairs({ "oldMark", "newMark", "footMark", "anchorMark" }) do
    if h[k] then
      pcall(vim.api.nvim_buf_del_extmark, S.buf, ns, h[k])
      h[k] = nil
    end
  end
end

local function pending()
  local n = 0
  for _, h in ipairs(S.hunks) do
    if not h.done then
      n = n + 1
    end
  end
  return n
end

local function refresh_qf()
  local qf = {}
  for i, h in ipairs(S.hunks) do
    if not h.done then
      local r = mark_range(h.oldMark) or mark_range(h.newMark)
      if r then
        table.insert(qf, {
          bufnr = S.buf,
          lnum = r[1] + 1,
          text = string.format("cx edit %d/%d", i, S.total),
        })
      end
    end
  end
  vim.fn.setqflist({}, " ", { title = "cx edits", items = qf })
end

local function finish()
  for _, h in ipairs(S.hunks) do
    del_marks(h)
  end
  vim.api.nvim_buf_clear_namespace(S.buf, ns, 0, -1)
  for _, lhs in ipairs({ "y", "n", "N", "a", "u", "q" }) do
    pcall(vim.keymap.del, "n", lhs, { buffer = S.buf })
  end
  pcall(vim.api.nvim_buf_call, S.buf, function()
    vim.cmd("silent! write")
  end)
  pcall(vim.fn.setqflist, {}, "r")
  local results, applied = {}, 0
  for _, h in ipairs(S.hunks) do
    local r = h.result or { applied = false }
    table.insert(results, r)
    if r.applied then
      applied = applied + 1
    end
  end
  vim.fn.writefile({ vim.json.encode({ results = results }) }, datadir .. "/edits-done.json")
  vim.notify(string.format("cx: %d/%d applied", applied, #results))
  S.hunks = nil
end

-- decorate paints one hunk: red over the old block, green over the (real)
-- new block, and a control footer.
local function decorate(h, idx, oldStart, insStart)
  if h.word then
    local p, oe, ne = split_diff(h.old[1], h.new[1])
    if oe > p then
      h.oldMark = vim.api.nvim_buf_set_extmark(S.buf, ns, oldStart, p, {
        end_col = oe,
        hl_group = "DiffDelete",
      })
    else
      h.oldMark = vim.api.nvim_buf_set_extmark(S.buf, ns, oldStart, 0, {
        end_row = oldStart + 1,
        hl_group = "DiffDelete",
        hl_eol = true,
      })
    end
    if ne > p then
      h.newMark = vim.api.nvim_buf_set_extmark(S.buf, ns, insStart, p, {
        end_col = ne,
        hl_group = "DiffAdd",
      })
    else
      h.newMark = vim.api.nvim_buf_set_extmark(S.buf, ns, insStart, 0, {
        end_row = insStart + 1,
        hl_group = "DiffAdd",
        hl_eol = true,
      })
    end
  else
    if h.delN > 0 then
      h.oldMark = vim.api.nvim_buf_set_extmark(S.buf, ns, oldStart, 0, {
        end_row = oldStart + h.delN,
        hl_group = "DiffDelete",
        hl_eol = true,
      })
    end
    if h.insN > 0 then
      h.newMark = vim.api.nvim_buf_set_extmark(S.buf, ns, insStart, 0, {
        end_row = insStart + h.insN,
        hl_group = "DiffAdd",
        hl_eol = true,
      })
    end
  end
  local footRow = h.insN > 0 and (insStart + h.insN - 1) or (oldStart + h.delN - 1)
  h.footMark = vim.api.nvim_buf_set_extmark(S.buf, ns, footRow, 0, {
    virt_lines = {
      {
        {
          string.format("cx %d/%d · y apply  n skip  N reject+note  a all  u undo  q finish", idx, S.total),
          "Comment",
        },
      },
    },
  })
end

-- covers reports whether an extmark's block contains a 1-based line.
local function covers(id, line)
  local r = mark_range(id)
  return r ~= nil and line >= r[1] + 1 and line <= r[2]
end

-- hunk_at returns the undecided hunk whose red or green block covers a
-- 1-based line, or nil. (Never ipairs over {oldMark, newMark}: either can
-- be nil for pure inserts/deletes, which truncates the table.)
local function hunk_at(line)
  for _, h in ipairs(S.hunks or {}) do
    if not h.done and (covers(h.oldMark, line) or covers(h.newMark, line)) then
      return h
    end
  end
  return nil
end

local function current_hunk()
  local win = vim.fn.bufwinid(S.buf)
  if win == -1 then
    return nil
  end
  return hunk_at(vim.api.nvim_win_get_cursor(win)[1])
end

local function goto_next_pending()
  local win = vim.fn.bufwinid(S.buf)
  if win == -1 then
    return
  end
  if hunk_at(vim.api.nvim_win_get_cursor(win)[1]) then
    return
  end
  for _, h in ipairs(S.hunks) do
    if not h.done then
      local r = mark_range(h.oldMark) or mark_range(h.newMark)
      if r then
        vim.api.nvim_win_set_cursor(win, { r[1] + 1, 0 })
        return
      end
    end
  end
end

-- settle removes one side of a hunk. keepNew=true keeps the green block
-- (apply); false keeps the red block (skip/reject).
local function settle(h, keepNew)
  local dropRange = keepNew and mark_range(h.oldMark) or mark_range(h.newMark)
  local keepRange = keepNew and mark_range(h.newMark) or mark_range(h.oldMark)
  if keepNew and h.delN == 0 then
    dropRange = nil -- pure insertion: nothing to drop
  end
  if not keepNew and h.insN == 0 then
    dropRange = nil -- pure deletion skipped: nothing to drop
  end
  h.removed = nil
  if dropRange then
    h.removed = vim.api.nvim_buf_get_lines(S.buf, dropRange[1], dropRange[2], false)
    vim.api.nvim_buf_set_lines(S.buf, dropRange[1], dropRange[2], false, {})
  end
  del_marks(h)
  -- invisible anchor over (or at) the surviving block, for undo positioning
  local start, rows = 0, 0
  if keepRange then
    start = keepRange[1]
    rows = keepRange[2] - keepRange[1]
    if dropRange and dropRange[1] < keepRange[1] then
      start = start - (dropRange[2] - dropRange[1])
    end
  elseif dropRange then
    start = dropRange[1]
  end
  local opts = {}
  if rows > 0 then
    opts.end_row = start + rows
  end
  h.anchorMark = vim.api.nvim_buf_set_extmark(S.buf, ns, start, 0, opts)
  h.keptRows = rows
  h.keepNew = keepNew
  h.done = true
end

local function undo_last()
  local h = table.remove(S.hist)
  if not h then
    vim.notify("cx: nothing to undo")
    return
  end
  local r = mark_range(h.anchorMark)
  local base = r and r[1] or 0
  local at = base
  if not h.keepNew and h.keptRows > 0 then
    at = base + h.keptRows -- green reinserts below the kept red block
  end
  if h.removed then
    vim.api.nvim_buf_set_lines(S.buf, at, at, false, h.removed)
  end
  del_marks(h)
  h.done = false
  h.result = nil
  local idx = 1
  for i, hh in ipairs(S.hunks) do
    if hh == h then
      idx = i
    end
  end
  decorate(h, idx, base, base + h.delN)
  refresh_qf()
  local win = vim.fn.bufwinid(S.buf)
  if win ~= -1 then
    vim.api.nvim_win_set_cursor(win, { base + 1, 0 })
  end
end

local function after_decision()
  refresh_qf()
  if pending() == 0 then
    finish()
  else
    goto_next_pending()
  end
end

local function decide(action)
  if not S.hunks then
    return
  end
  if action == "undo" then
    undo_last()
    return
  end
  if action == "all" then
    for _, h in ipairs(S.hunks) do
      if not h.done then
        settle(h, true)
        h.result = { applied = true }
        table.insert(S.hist, h)
      end
    end
    finish()
    return
  end
  if action == "quit" then
    for _, h in ipairs(S.hunks) do
      if not h.done then
        settle(h, false)
        h.result = { applied = false }
      end
    end
    finish()
    return
  end

  local h = current_hunk()
  if not h then
    vim.notify("cx: move the cursor onto an edit (]q)")
    return
  end
  if action == "apply" then
    settle(h, true)
    h.result = { applied = true }
    table.insert(S.hist, h)
    after_decision()
  elseif action == "skip" then
    settle(h, false)
    h.result = { applied = false }
    table.insert(S.hist, h)
    after_decision()
  elseif action == "reject" then
    vim.ui.input({ prompt = "why? " }, function(reason)
      settle(h, false)
      h.result = { applied = false, reason = reason or "" }
      if reason and reason ~= "" then
        h.result.reported = true
        local f = io.open(datadir .. "/reject-now.jsonl", "a")
        if f then
          f:write(vim.json.encode({
            reason = reason,
            search = table.concat(h.search, "\n"),
            replace = table.concat(h.replace, "\n"),
          }) .. "\n")
          f:close()
        end
      end
      table.insert(S.hist, h)
      after_decision()
    end)
  end
end

-- CxChecktime: cx pokes this after writing files so buffers hot-reload.
function CxChecktime()
  vim.cmd("silent! checktime")
  return 1
end

-- CxReview: entry point, called by cx over --remote after writing edits.json.
function CxReview()
  local ok, raw = pcall(vim.fn.readfile, datadir .. "/edits.json")
  if not ok or #raw == 0 then
    return 0
  end
  local req = vim.json.decode(table.concat(raw, "\n"))
  if not req or not req.edits or #req.edits == 0 then
    return 0
  end

  local buf = vim.fn.bufnr(req.file)
  if buf == -1 then
    vim.cmd("edit " .. vim.fn.fnameescape(req.file))
    buf = vim.api.nvim_get_current_buf()
  else
    local win = vim.fn.bufwinid(buf)
    if win ~= -1 then
      vim.api.nvim_set_current_win(win)
    else
      vim.api.nvim_set_current_buf(buf)
    end
  end
  vim.cmd("silent! checktime")

  S.buf = buf
  S.total = #req.edits
  S.hunks = {}
  S.hist = {}

  -- locate every hunk against the pristine buffer first
  for _, e in ipairs(req.edits) do
    local h = { search = lines_of(e.search), replace = lines_of(e.replace) }
    h.start = locate(buf, h.search)
    if not h.start then
      h.done = true
      h.result = { applied = false, reason = "not found in buffer" }
    else
      local p, sfx = common_affixes(h.search, h.replace)
      h.p = p
      h.delN = #h.search - p - sfx
      h.insN = #h.replace - p - sfx
      h.old = {}
      for k = p + 1, #h.search - sfx do
        table.insert(h.old, h.search[k])
      end
      h.new = {}
      for k = p + 1, #h.replace - sfx do
        table.insert(h.new, h.replace[k])
      end
      h.word = h.delN == 1 and h.insN == 1
        and is_ascii(h.old[1] or "") and is_ascii(h.new[1] or "")
    end
    table.insert(S.hunks, h)
  end

  -- insert the green blocks bottom-up so earlier positions stay valid
  local order = {}
  for i, h in ipairs(S.hunks) do
    if not h.done then
      table.insert(order, i)
    end
  end
  table.sort(order, function(a, b)
    return S.hunks[a].start > S.hunks[b].start
  end)
  for _, i in ipairs(order) do
    local h = S.hunks[i]
    local oldStart = h.start + h.p
    local insStart = oldStart + h.delN
    if #h.new > 0 then
      vim.api.nvim_buf_set_lines(buf, insStart, insStart, false, h.new)
    end
    decorate(h, i, oldStart, insStart)
  end

  refresh_qf()
  if pending() == 0 then
    finish()
    return 1
  end
  goto_next_pending()

  for lhs, action in pairs({ y = "apply", n = "skip", N = "reject", a = "all", u = "undo", q = "quit" }) do
    vim.keymap.set("n", lhs, function()
      decide(action)
    end, { buffer = buf, nowait = true })
  end

  local word = S.total == 1 and "edit" or "edits"
  vim.notify(string.format("cx: %d %s · y/n/N/a/q · ]q next", S.total, word))
  return 1
end
