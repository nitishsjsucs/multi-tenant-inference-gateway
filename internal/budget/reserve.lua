-- Atomically check and reserve a request's token cost.
--
-- KEYS[1] current minute bucket   KEYS[2] previous minute bucket
-- KEYS[3] UTC day bucket
-- ARGV[1] cost   ARGV[2] tokens/minute limit (0 = off)   ARGV[3] tokens/day limit (0 = off)
-- ARGV[4] weight of the previous minute bucket, 1 - elapsed/60s
-- ARGV[5] minute bucket TTL (ms)   ARGV[6] day bucket TTL (s)
--
-- Returns {allowed, reason, current, previous, day}; reason is 0 (ok),
-- 1 (minute limit) or 2 (day limit). Counts are after the reservation when
-- allowed and before it otherwise.
local cost = tonumber(ARGV[1])
local tpm = tonumber(ARGV[2])
local tpd = tonumber(ARGV[3])
local weight = tonumber(ARGV[4])

local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
local prev = tonumber(redis.call('GET', KEYS[2]) or '0')
local day = tonumber(redis.call('GET', KEYS[3]) or '0')

if tpm > 0 and math.floor(prev * weight) + cur + cost > tpm then
  return {0, 1, cur, prev, day}
end
if tpd > 0 and day + cost > tpd then
  return {0, 2, cur, prev, day}
end

cur = redis.call('INCRBY', KEYS[1], cost)
redis.call('PEXPIRE', KEYS[1], ARGV[5])
day = redis.call('INCRBY', KEYS[3], cost)
redis.call('EXPIRE', KEYS[3], ARGV[6])
return {1, 0, cur, prev, day}
