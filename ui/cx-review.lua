-- cx-review.lua — in-neovim review UI for cx doc edits.
-- Written by cx to ~/.local/share/cx/cx-review.lua on editor launch; do not edit.
--
-- Proposed text is inserted as REAL buffer lines (green) below the lines it
-- replaces (red). Decisions are plain buffer edits: y deletes the red block
-- (applies), n/N deletes the green block (skips/rejects), a applies all,
-- q finishes. There is NO custom undo: plain vim u reverts a decision, the
-- highlights restore with the text (extmark undo_restore), and a TextChanged
-- autocmd repaints the footer/quickfix so the hunk is visibly pending again.
--
-- Hunk state is DERIVED from the buffer: red gone = applied, green gone =
-- skipped, both present = pending. The file is written when the review
-- finishes; results land in edits-done.json for cx.

local ns = vim.api.nvim_create_namespace("cx_review")        -- invisible tracking marks
local paint = vim.api.nvim_create_namespace("cx_review_paint") -- highlights + footers, redrawn
local aug = vim.api.nvim_create_augroup("cx_review", { clear = true })
local datadir = vim.fn.expand("~/.local/share/cx")

local S = { hunks = nil, buf = nil, total = 0 }

-- blend an 0xRRGGBB color toward a target by factor f (0..1)
local function blend(c, target, f)
  local r = math.floor(c / 65536) % 256
  local g = math.floor(c / 256) % 256
  local b = c % 256
  local tr = math.floor(target / 65536) % 256
  local tg = math.floor(target / 256) % 256
  local tb = target % 256
  r = math.floor(r + (tr - r) * f)
  g = math.floor(g + (tg - g) * f)
  b = math.floor(b + (tb - b) * f)
  return r * 65536 + g * 256 + b
end

-- setup_word_hl derives word-emphasis groups from the colorscheme's own
-- diff colors: the changed span is a stronger shade of the SAME hue
-- (git-delta / GitHub convention), never a different color like DiffText.
local function setup_word_hl()
  -- Bold-only was too subtle. Emphasize by INVERTING the line's diff bg
  -- as the changed span's fg, plus underline + bold. Whatever fg the base
  -- theme uses on the diff line becomes the span's bg — maximum contrast
  -- against the surrounding red/green line.
  for base, name in pairs({ DiffDelete = "CxWordDel", DiffAdd = "CxWordAdd" }) do
    local h = vim.api.nvim_get_hl(0, { name = base, link = false })
    local spec = { bold = true, underline = true, reverse = true }
    if h.fg then
      spec.fg = h.fg
    end
    if h.bg then
      spec.bg = h.bg
    end
    vim.api.nvim_set_hl(0, name, spec)
  end
end

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

-- split_diff: changed byte span between two single ASCII lines, snapped to
-- word boundaries.
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

-- mark_info returns {startRow, endRowExclusive} for a live extmark, or nil
-- when the mark is gone/invalidated (its text was deleted).
local function mark_info(id)
  if not id then
    return nil
  end
  local m = vim.api.nvim_buf_get_extmark_by_id(S.buf, ns, id, { details = true })
  if not m or #m == 0 then
    return nil
  end
  if m[3] and m[3].invalid then
    return nil
  end
  local endRow = (m[3] and m[3].end_row) or m[1]
  if endRow <= m[1] then
    endRow = m[1] + 1
  end
  return { m[1], endRow }
end

-- state derives a hunk's status from what is actually in the buffer.
local function state(h)
  if h.notfound then
    return "notfound"
  end
  local red = h.delN > 0 and mark_info(h.redMark) ~= nil
  local green = h.insN > 0 and mark_info(h.greenMark) ~= nil
  if h.delN > 0 and h.insN > 0 then
    if red and green then
      return "pending"
    elseif green then
      return "applied"
    end
    return "skipped"
  elseif h.delN == 0 then -- pure insertion
    if not green then
      return "skipped"
    elseif h.accepted then
      return "applied"
    end
    return "pending"
  else -- pure deletion
    if not red then
      return "applied"
    elseif h.skipped then
      return "skipped"
    end
    return "pending"
  end
end

local function pending()
  local n = 0
  for _, h in ipairs(S.hunks) do
    if state(h) == "pending" then
      n = n + 1
    end
  end
  return n
end

local function hunk_row(h)
  local r = mark_info(h.redMark) or mark_info(h.greenMark)
  return r and r[1] or nil
end

-- paint_line stamps a per-line line_hl_group extmark on every row in
-- [startRow, endRow). That's the ONE thing treesitter/render-markdown
-- can't override with a token-level bg — line_hl_group paints the whole
-- line including tokens like ##, -, *, ● that syntax plugins style
-- with their own backgrounds.
local function paint_line_range(startRow, endRow, hl)
  for r = startRow, endRow - 1 do
    vim.api.nvim_buf_set_extmark(S.buf, paint, r, 0, {
      line_hl_group = hl,
      priority = 10000,
    })
  end
end

-- paint_hunk draws highlights for one pending hunk from its live tracking
-- ranges (word-level for single-line changes).
local function paint_hunk(h, idx)
  local red = mark_info(h.redMark)
  local green = mark_info(h.greenMark)
  if h.word and red and green then
    -- GitHub-style: whole lines carry the diff background so it reads as a
    -- standard line diff, and the changed span gets a stronger overlay
    paint_line_range(red[1], red[2], "DiffDelete")
    paint_line_range(green[1], green[2], "DiffAdd")
    vim.api.nvim_buf_set_extmark(S.buf, paint, red[1], 0, { end_row = red[2], hl_group = "DiffDelete", hl_eol = true, priority = 10000 })
    vim.api.nvim_buf_set_extmark(S.buf, paint, green[1], 0, { end_row = green[2], hl_group = "DiffAdd", hl_eol = true, priority = 10000 })
    local wp, oe, ne = split_diff(h.old[1], h.new[1])
    if oe > wp then
      vim.api.nvim_buf_set_extmark(S.buf, paint, red[1], wp, { end_col = oe, hl_group = "CxWordDel", priority = 5000, strict = false })
    end
    if ne > wp then
      vim.api.nvim_buf_set_extmark(S.buf, paint, green[1], wp, { end_col = ne, hl_group = "CxWordAdd", priority = 5000, strict = false })
    end
  else
    if red then
      paint_line_range(red[1], red[2], "DiffDelete")
      vim.api.nvim_buf_set_extmark(S.buf, paint, red[1], 0, { end_row = red[2], hl_group = "DiffDelete", hl_eol = true, priority = 10000 })
    end
    if green then
      paint_line_range(green[1], green[2], "DiffAdd")
      vim.api.nvim_buf_set_extmark(S.buf, paint, green[1], 0, { end_row = green[2], hl_group = "DiffAdd", hl_eol = true, priority = 10000 })
    end
  end
  local anchor = green or red
  if anchor then
    vim.api.nvim_buf_set_extmark(S.buf, paint, anchor[2] - 1, 0, {
      virt_lines = {
        {
          {
            string.format("cx %d/%d · y apply  n skip  N reject+note  a all  q finish", idx, S.total),
            "Comment",
          },
        },
      },
    })
  end
end

-- repaint redraws every pending hunk and refreshes the quickfix list from
-- derived state. Runs after decisions AND on TextChanged, so vim-native
-- undo brings the full review UX back; decided hunks lose their coloring
-- instantly.
local function repaint()
  if not S.hunks then
    return
  end
  vim.api.nvim_buf_clear_namespace(S.buf, paint, 0, -1)
  local qf = {}
  for i, h in ipairs(S.hunks) do
    if state(h) == "pending" then
      paint_hunk(h, i)
      local row = hunk_row(h)
      if row then
        table.insert(qf, {
          bufnr = S.buf,
          lnum = row + 1,
          text = string.format("cx edit %d/%d", i, S.total),
        })
      end
    end
  end
  pcall(vim.fn.setqflist, {}, " ", { title = "cx edits", items = qf })
end

local function covers(id, line)
  local r = mark_info(id)
  return r ~= nil and line >= r[1] + 1 and line <= r[2]
end

local function hunk_at(line)
  for _, h in ipairs(S.hunks or {}) do
    if state(h) == "pending" and (covers(h.redMark, line) or covers(h.greenMark, line)) then
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
    if state(h) == "pending" then
      local row = hunk_row(h)
      if row then
        vim.api.nvim_win_set_cursor(win, { row + 1, 0 })
        return
      end
    end
  end
end

-- suppress_plugins disables markdown-rendering plugins that paint per-token
-- backgrounds on #, ##, -, *, ● at priorities that beat line_hl_group.
-- Returns a restore fn called on finish/abort.
local function suppress_plugins(buf)
  local restores = {}
  local ok, rm = pcall(require, "render-markdown")
  if ok and rm then
    if type(rm.buf_disable) == "function" then
      pcall(rm.buf_disable, buf)
      table.insert(restores, function() pcall(rm.buf_enable, buf) end)
    elseif type(rm.disable) == "function" then
      pcall(rm.disable)
      table.insert(restores, function() pcall(rm.enable) end)
    end
  end
  return function()
    for _, r in ipairs(restores) do r() end
  end
end

local function finish()
  local results, applied = {}, 0
  for _, h in ipairs(S.hunks) do
    local st = state(h)
    local r
    if st == "applied" then
      r = { applied = true }
      applied = applied + 1
    elseif st == "notfound" then
      r = { applied = false, reason = "not found in buffer" }
    else
      r = { applied = false }
      if h.reason and h.reason ~= "" then
        r.reason = h.reason
        r.reported = h.reported or nil
      end
    end
    table.insert(results, r)
  end
  vim.api.nvim_buf_clear_namespace(S.buf, ns, 0, -1)
  vim.api.nvim_buf_clear_namespace(S.buf, paint, 0, -1)
  vim.api.nvim_clear_autocmds({ group = aug, buffer = S.buf })
  for _, lhs in ipairs({ "y", "n", "N", "a", "q" }) do
    pcall(vim.keymap.del, "n", lhs, { buffer = S.buf })
  end
  pcall(vim.api.nvim_buf_call, S.buf, function()
    vim.cmd("silent! write")
  end)
  if vim.bo[S.buf].modified then
    -- write failed (readonly, permissions): the file does NOT contain the
    -- accepted edits, so don't tell cx (and the model) that it does
    for _, r in ipairs(results) do
      if r.applied then
        r.applied = false
        r.reason = "buffer write failed"
      end
    end
    applied = 0
    vim.notify("cx: buffer write failed, edits not applied to disk")
  end
  pcall(vim.fn.setqflist, {}, "r")
  vim.fn.writefile({ vim.json.encode({ results = results }) }, datadir .. "/edits-done.json")
  vim.notify(string.format("cx: %d/%d applied", applied, #results))
  if S.restore_plugins then
    S.restore_plugins()
    S.restore_plugins = nil
  end
  S.hunks = nil
end

local function delete_block(id)
  local r = mark_info(id)
  if r then
    vim.api.nvim_buf_set_lines(S.buf, r[1], r[2], false, {})
  end
end

local function post_decide()
  repaint()
  if pending() == 0 then
    finish()
  else
    goto_next_pending()
  end
end

local function do_apply(h)
  if h.delN > 0 then
    delete_block(h.redMark)
  else
    h.accepted = true
  end
  h.reason = nil
end

local function do_skip(h)
  if h.insN > 0 then
    delete_block(h.greenMark)
  else
    h.skipped = true
  end
end

-- ask_reason opens a bordered floating buffer to capture a rejection note.
-- vim.ui.input redraws its cmdline prefix on every wrapped row and produces
-- ghost text with long input; a real buffer gets normal wrap/edit/paste.
-- The user gets a normal insert-mode buffer:
--   <CR> in NORMAL mode (esc first) submits    <Esc><Esc> or q  cancels
local function ask_reason(cb)
  local buf = vim.api.nvim_create_buf(false, true)
  vim.bo[buf].buftype = "nofile"
  vim.bo[buf].bufhidden = "wipe"
  vim.bo[buf].swapfile = false

  local width = math.min(80, vim.o.columns - 4)
  local win = vim.api.nvim_open_win(buf, true, {
    relative = "editor",
    row = math.floor(vim.o.lines / 2 - 3),
    col = math.floor((vim.o.columns - width) / 2),
    width = width,
    height = 6,
    style = "minimal",
    border = "rounded",
    title = " why reject this edit? (esc esc to cancel, esc <CR> to submit) ",
    title_pos = "center",
  })
  vim.wo[win].wrap = true
  vim.wo[win].linebreak = true
  vim.cmd("startinsert")

  local closed = false
  local function close(reason)
    if closed then
      return
    end
    closed = true
    if vim.api.nvim_win_is_valid(win) then
      vim.api.nvim_win_close(win, true)
    end
    cb(reason)
  end

  vim.keymap.set("n", "<CR>", function()
    local lines = vim.api.nvim_buf_get_lines(buf, 0, -1, false)
    close(vim.trim(table.concat(lines, "\n")))
  end, { buffer = buf })
  vim.keymap.set("n", "<Esc>", function() close(nil) end, { buffer = buf })
  vim.keymap.set("n", "q", function() close(nil) end, { buffer = buf })
  -- if the window loses focus, treat it as a cancel
  vim.api.nvim_create_autocmd("WinLeave", {
    buffer = buf,
    once = true,
    callback = function() close(nil) end,
  })
end

local function decide(action)
  if not S.hunks then
    return
  end
  if action == "all" then
    for _, h in ipairs(S.hunks) do
      if state(h) == "pending" then
        do_apply(h)
      end
    end
    post_decide()
    return
  end
  if action == "quit" then
    for _, h in ipairs(S.hunks) do
      if state(h) == "pending" then
        do_skip(h)
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
    do_apply(h)
    post_decide()
  elseif action == "skip" then
    do_skip(h)
    post_decide()
  elseif action == "reject" then
    ask_reason(function(reason)
      if reason == nil then
        -- cancel: the hunk stays pending with its diff intact
        vim.notify("cx: reject cancelled")
        return
      end
      do_skip(h)
      h.reason = reason
      if reason ~= "" then
        h.reported = true
        local ev = {
          reason = reason,
          search = table.concat(h.search, "\n"),
          replace = table.concat(h.replace, "\n"),
        }
        -- Fold any pending editor highlight into the rejection so the
        -- model can act on it while retrying, without needing a separate
        -- chat message
        local ok, sel = pcall(vim.fn.readfile, datadir .. "/selection.txt")
        if ok and #sel >= 3 then
          local from, to = sel[2]:match("(%d+)-(%d+)")
          ev.selection_file = sel[1]
          ev.selection_from = tonumber(from)
          ev.selection_to = tonumber(to)
          ev.selection_text = table.concat({ unpack(sel, 3) }, "\n")
          -- consume so the next chat message doesn't re-send it
          pcall(os.remove, datadir .. "/selection.txt")
        end
        local f = io.open(datadir .. "/reject-now.jsonl", "a")
        if f then
          f:write(vim.json.encode(ev) .. "\n")
          f:close()
        end
      end
      post_decide()
    end)
  end
end

-- decorate creates the invisible tracking marks for a hunk. They carry no
-- highlight (paint_hunk draws those); invalidate=true reports the block as
-- gone when deleted, and undo_restore (default) revives it on vim undo.
local function decorate(h, oldStart, insStart)
  local rN = h.redPaintN or h.delN
  local gN = h.greenPaintN or h.insN
  if rN > 0 then
    h.redMark = vim.api.nvim_buf_set_extmark(S.buf, ns, oldStart, 0, {
      end_row = oldStart + rN,
      invalidate = true,
    })
  end
  if gN > 0 then
    h.greenMark = vim.api.nvim_buf_set_extmark(S.buf, ns, insStart, 0, {
      end_row = insStart + gN,
      invalidate = true,
    })
  end
end

-- CxApplyAll accepts every pending hunk from the outside (cx tool call).
-- Silent no-op when there's no active review.
function CxApplyAll()
  if not S.hunks then
    return 0
  end
  for _, h in ipairs(S.hunks) do
    if state(h) == "pending" then
      do_apply(h)
    end
  end
  repaint()
  if pending() == 0 then
    finish()
  end
  return 1
end

-- CxApplyIndex accepts one pending hunk by 1-based index.
function CxApplyIndex(i)
  if not S.hunks or i < 1 or i > #S.hunks then return 0 end
  local h = S.hunks[i]
  if state(h) ~= "pending" then return 0 end
  do_apply(h)
  repaint()
  if pending() == 0 then finish() end
  return 1
end

-- CxRejectIndex rejects one pending hunk by 1-based index with an optional
-- reason. Writes a reject-now event so cx can auto-retry (same as N).
function CxRejectIndex(i, reason)
  if not S.hunks or i < 1 or i > #S.hunks then return 0 end
  local h = S.hunks[i]
  if state(h) ~= "pending" then return 0 end
  do_skip(h)
  h.reason = reason or ""
  if reason and reason ~= "" then
    h.reported = true
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
  repaint()
  if pending() == 0 then finish() end
  return 1
end

-- CxAbort discards a live review: pending proposal (green) blocks are
-- removed, decorations and keymaps cleaned up, no results written. Called
-- by cx when it abandons a review, and by CxReview before starting a new
-- one over a stale session.
function CxAbort()
  if not S.hunks or not vim.api.nvim_buf_is_valid(S.buf or -1) then
    S.hunks = nil
    return 0
  end
  for _, h in ipairs(S.hunks) do
    if state(h) == "pending" and h.insN and h.insN > 0 then
      local r = mark_info(h.greenMark)
      if r then
        pcall(vim.api.nvim_buf_set_lines, S.buf, r[1], r[2], false, {})
      end
    end
  end
  vim.api.nvim_buf_clear_namespace(S.buf, ns, 0, -1)
  vim.api.nvim_buf_clear_namespace(S.buf, paint, 0, -1)
  pcall(vim.api.nvim_clear_autocmds, { group = aug, buffer = S.buf })
  for _, lhs in ipairs({ "y", "n", "N", "a", "q" }) do
    pcall(vim.keymap.del, "n", lhs, { buffer = S.buf })
  end
  pcall(vim.fn.setqflist, {}, "r")
  if S.restore_plugins then
    S.restore_plugins()
    S.restore_plugins = nil
  end
  S.hunks = nil
  vim.notify("cx: review discarded")
  return 1
end

function CxChecktime()
  vim.cmd("silent! checktime")
  return 1
end

-- CxSyncDocs: cx calls this before every prompt so unsaved buffer changes in
-- connected docs reach disk (and therefore the model). Paths in docs.txt.
function CxSyncDocs()
  local ok, paths = pcall(vim.fn.readfile, datadir .. "/docs.txt")
  if not ok then
    return 0
  end
  local n = 0
  for _, path in ipairs(paths) do
    if path ~= "" then
      local buf = vim.fn.bufnr(path)
      -- never save the buffer under active review: it holds BOTH the old
      -- and proposed lines, which must not leak to disk
      if S.hunks and buf == S.buf then
        buf = -1
      end
      if buf ~= -1 and vim.bo[buf].modified then
        pcall(vim.api.nvim_buf_call, buf, function()
          vim.cmd("silent! write")
        end)
        n = n + 1
      end
    end
  end
  return n
end

-- prepare_hunk turns a raw {search, replace} edit into the fully-populated
-- hunk table (delN, insN, pivot handling, word-diff eligibility). Returns
-- a notfound-marked hunk when the search can't be located.
function prepare_hunk(buf, e)
  local h = { search = lines_of(e.search), replace = lines_of(e.replace) }
  h.start = locate(buf, h.search)
  if not h.start then
    h.notfound = true
    return h
  end
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

  -- Pivot detection (see CxReview comments).
  local pivotK = 0
  local minLen = math.min(#h.old, #h.new)
  for k = 1, minLen do
    local match = true
    for i = 1, k do
      if h.old[#h.old - k + i] ~= h.new[i] then
        match = false
        break
      end
    end
    if match then
      pivotK = k
    end
  end
  h.redPaintN = h.delN
  h.greenPaintN = h.insN
  h.newToInsert = h.new
  if pivotK > 0 and pivotK < #h.new and pivotK < #h.old then
    h.redPaintN = h.delN - pivotK
    h.greenPaintN = h.insN - pivotK
    h.newToInsert = {}
    for k = pivotK + 1, #h.new do
      table.insert(h.newToInsert, h.new[k])
    end
    h.word = false
  end
  return h
end

-- CxAddEdits appends new edits to the CURRENTLY LIVE review instead of
-- starting a fresh one. Used by cx when a retry response arrives during
-- an open review: previous decisions stay put, remaining pending edits
-- stay pending, and the new revised edits appear alongside them as new
-- pending hunks. Returns the number of hunks actually added.
function CxAddEdits()
  if not S.hunks or not S.buf or not vim.api.nvim_buf_is_valid(S.buf) then
    return 0
  end
  local ok, raw = pcall(vim.fn.readfile, datadir .. "/edits.json")
  if not ok or #raw == 0 then
    return 0
  end
  local req = vim.json.decode(table.concat(raw, "\n"))
  if not req or not req.edits or #req.edits == 0 then
    return 0
  end
  local added = 0
  -- Insert bottom-up by locate position so earlier insertions don't shift
  -- later ones' target rows.
  local prepared = {}
  for _, e in ipairs(req.edits) do
    table.insert(prepared, prepare_hunk(S.buf, e))
  end
  table.sort(prepared, function(a, b)
    return (a.start or -1) > (b.start or -1)
  end)
  for _, h in ipairs(prepared) do
    if not h.notfound then
      local oldStart = h.start + h.p
      local insStart = oldStart + h.delN
      if #h.newToInsert > 0 then
        pcall(vim.api.nvim_buf_set_lines, S.buf, insStart, insStart, false, h.newToInsert)
      end
      decorate(h, oldStart, insStart)
    end
    table.insert(S.hunks, h)
    S.total = S.total + 1
    added = added + 1
  end
  repaint()
  vim.notify(string.format("cx: %d more edit%s added to the review", added, added == 1 and "" or "s"))
  return added
end

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
  if vim.bo[buf].modified then
    pcall(vim.api.nvim_buf_call, buf, function()
      vim.cmd("silent! write")
    end)
  end

  if S.hunks then
    CxAbort() -- a previous review is still open (abandoned session): discard it
  end

  setup_word_hl()

  S.buf = buf
  S.total = #req.edits
  S.hunks = {}
  S.restore_plugins = suppress_plugins(buf)

  for _, e in ipairs(req.edits) do
    table.insert(S.hunks, prepare_hunk(buf, e))
  end

  -- Insert green blocks bottom-up so earlier positions stay valid. Any
  -- error mid-mutation would leave orphan proposal text in the buffer, so
  -- the whole phase is atomic: on failure, discard and report cleanly.
  local ok = pcall(function()
    local order = {}
    for i, h in ipairs(S.hunks) do
      if not h.notfound then
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
      local toInsert = h.newToInsert or h.new
      if #toInsert > 0 then
        vim.api.nvim_buf_set_lines(buf, insStart, insStart, false, toInsert)
      end
      decorate(h, oldStart, insStart)
    end
  end)
  if not ok then
    pcall(CxAbort)
    return 0
  end

  repaint()
  if pending() == 0 then
    finish()
    return 1
  end
  goto_next_pending()

  for lhs, action in pairs({ y = "apply", n = "skip", N = "reject", a = "all", q = "quit" }) do
    vim.keymap.set("n", lhs, function()
      decide(action)
    end, { buffer = buf, nowait = true })
  end

  -- vim-native undo/redo (or any edit) repaints the review state
  vim.api.nvim_clear_autocmds({ group = aug, buffer = buf })
  vim.api.nvim_create_autocmd({ "TextChanged", "TextChangedI" }, {
    group = aug,
    buffer = buf,
    callback = function()
      if S.hunks then
        repaint()
      end
    end,
  })

  local word = S.total == 1 and "edit" or "edits"
  vim.notify(string.format("cx: %d %s · y/n/N/a/q · ]q next", S.total, word))
  return 1
end
