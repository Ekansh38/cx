-- cx-review.lua — in-neovim review UI for cx doc edits.
-- Written by cx to ~/.local/share/cx/cx-review.lua on editor launch; do not edit.
--
-- All proposed hunks render at once as inline diffs (old text red, proposed
-- text green virtual lines) and land in the quickfix list. With the cursor on
-- a hunk: y apply, n skip, N reject with a note, a apply all, q quit.
-- Decisions are written to edits-done.json for cx to pick up.

local ns = vim.api.nvim_create_namespace("cx_review")
local datadir = vim.fn.expand("~/.local/share/cx")

local S = { hunks = nil, buf = nil, total = 0 }

local function lines_of(s)
  return vim.split(s, "\n", { plain = true })
end

-- locate returns the 0-based start line where lines match, or nil.
-- Falls back to comparing with trailing whitespace stripped.
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

-- split_diff finds the changed byte span between two single lines.
-- Returns prefix_len, old_end, new_end (bytes). Only safe for pure ASCII.
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
  return p, oe, ne
end

local function is_ascii(s)
  return not s:find("[\128-\255]")
end

local function win_width(buf)
  local win = vim.fn.bufwinid(buf)
  local w = 80
  if win ~= -1 then
    w = vim.api.nvim_win_get_width(win)
  end
  return math.max(w - 6, 20)
end

-- wrap_virt breaks one logical line into width-sized virt_lines chunks.
local function wrap_virt(line, width, hl)
  local out = {}
  if line == "" then
    return { { { " ", hl } } }
  end
  local i = 1
  while i <= vim.fn.strchars(line) do
    local chunk = vim.fn.strcharpart(line, i - 1, width)
    table.insert(out, { { chunk, hl } })
    i = i + width
  end
  return out
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

local function clear_marks()
  if S.buf and vim.api.nvim_buf_is_valid(S.buf) then
    vim.api.nvim_buf_clear_namespace(S.buf, ns, 0, -1)
  end
end

local function finish()
  clear_marks()
  for _, lhs in ipairs({ "y", "n", "N", "a", "q" }) do
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

-- hunk_at returns the undecided hunk covering a 1-based line, or nil.
local function hunk_at(line)
  for _, h in ipairs(S.hunks or {}) do
    if not h.done and h.start and line >= h.start + 1 and line <= h.start + #h.search then
      return h
    end
  end
  return nil
end

-- render draws every undecided hunk, refreshes the quickfix list, and
-- finishes when nothing is left. Re-locates hunks each pass since earlier
-- applies shift line numbers.
local function render()
  clear_marks()
  local qf = {}
  local width = win_width(S.buf)
  for i, h in ipairs(S.hunks) do
    if not h.done then
      local start = locate(S.buf, h.search)
      if not start then
        h.done = true
        h.result = { applied = false, reason = "not found in buffer" }
      else
        h.start = start
        local virt = {}
        local oneline = #h.search == 1 and #h.replace == 1
          and is_ascii(h.search[1]) and is_ascii(h.replace[1])
        if oneline then
          -- word-level: highlight only the changed span
          local p, oe, ne = split_diff(h.search[1], h.replace[1])
          if oe > p then
            vim.api.nvim_buf_set_extmark(S.buf, ns, start, p, {
              end_col = oe,
              hl_group = "DiffDelete",
            })
          end
          local new = h.replace[1]
          local chunks = {}
          if p > 0 then
            table.insert(chunks, { new:sub(1, p), "Comment" })
          end
          if ne > p then
            table.insert(chunks, { new:sub(p + 1, ne), "DiffAdd" })
          end
          if ne < #new then
            table.insert(chunks, { new:sub(ne + 1), "Comment" })
          end
          if #chunks == 0 then
            chunks = { { new, "DiffAdd" } }
          end
          virt = { chunks }
        else
          vim.api.nvim_buf_set_extmark(S.buf, ns, start, 0, {
            end_row = start + #h.search,
            hl_group = "DiffDelete",
            hl_eol = true,
          })
          for _, l in ipairs(h.replace) do
            vim.list_extend(virt, wrap_virt(l, width, "DiffAdd"))
          end
        end
        table.insert(virt, { { string.format("cx %d/%d", i, S.total), "Comment" } })
        vim.api.nvim_buf_set_extmark(S.buf, ns, start + #h.search - 1, 0, { virt_lines = virt })
        table.insert(qf, {
          bufnr = S.buf,
          lnum = start + 1,
          text = string.format("cx edit %d/%d", i, S.total),
        })
      end
    end
  end
  vim.fn.setqflist({}, " ", { title = "cx edits", items = qf })
  if pending() == 0 then
    finish()
    return
  end
  -- keep the cursor on a hunk so y/n/N always have a target
  local win = vim.fn.bufwinid(S.buf)
  if win ~= -1 then
    local line = vim.api.nvim_win_get_cursor(win)[1]
    if not hunk_at(line) then
      for _, h in ipairs(S.hunks) do
        if not h.done and h.start then
          vim.api.nvim_win_set_cursor(win, { h.start + 1, 0 })
          break
        end
      end
    end
  end
end

local function apply(h)
  vim.api.nvim_buf_set_lines(S.buf, h.start, h.start + #h.search, false, h.replace)
  h.done = true
  h.result = { applied = true }
end

local function current_hunk()
  local win = vim.fn.bufwinid(S.buf)
  if win == -1 then
    return nil
  end
  return hunk_at(vim.api.nvim_win_get_cursor(win)[1])
end

local function decide(action)
  if not S.hunks then
    return
  end
  if action == "all" then
    for _, h in ipairs(S.hunks) do
      if not h.done then
        local start = locate(S.buf, h.search)
        if start then
          h.start = start
          apply(h)
        else
          h.done = true
          h.result = { applied = false, reason = "not found in buffer" }
        end
      end
    end
    render()
    return
  end
  if action == "quit" then
    for _, h in ipairs(S.hunks) do
      if not h.done then
        h.done = true
        h.result = { applied = false }
      end
    end
    render()
    return
  end

  local h = current_hunk()
  if not h then
    vim.notify("cx: move the cursor onto an edit (]q)")
    return
  end
  if action == "apply" then
    apply(h)
    render()
  elseif action == "skip" then
    h.done = true
    h.result = { applied = false }
    render()
  elseif action == "reject" then
    vim.ui.input({ prompt = "why? " }, function(reason)
      h.done = true
      h.result = { applied = false, reason = reason or "" }
      render()
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

  -- Focus (or open) the target file
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
  for _, e in ipairs(req.edits) do
    table.insert(S.hunks, { search = lines_of(e.search), replace = lines_of(e.replace) })
  end

  for lhs, action in pairs({ y = "apply", n = "skip", N = "reject", a = "all", q = "quit" }) do
    vim.keymap.set("n", lhs, function()
      decide(action)
    end, { buffer = buf, nowait = true })
  end

  render()
  if S.hunks then
    vim.notify(string.format("cx: %d edits · y/n/N/a/q · ]q next", S.total))
  end
  return 1
end
