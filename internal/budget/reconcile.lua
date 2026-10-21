-- Adjust the buckets a reservation was charged to by ARGV[1] tokens
-- (actual - reserved: negative refunds, positive charges an overrun).
-- Buckets that already expired are left alone; counts never go below zero.
--
-- KEYS[1] minute bucket of the reservation   KEYS[2] day bucket
local delta = tonumber(ARGV[1])
for _, key in ipairs(KEYS) do
  if redis.call('EXISTS', key) == 1 then
    local v = redis.call('INCRBY', key, delta)
    if v < 0 then
      redis.call('INCRBY', key, -v)
    end
  end
end
return 1
