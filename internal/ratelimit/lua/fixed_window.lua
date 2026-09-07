-- Increment a fixed-window counter and report whether the caller is within its limit.
--
-- This is one script rather than INCR followed by PEXPIRE because those are two round trips: a
-- client that dies between them leaves a counter with no expiry, and that key never resets. The
-- caller is then locked out permanently by a counter nothing will ever clear.

local key = KEYS[1]
local limit = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])

local count = redis.call('INCR', key)

if count == 1 then
    redis.call('PEXPIRE', key, window_ms)
end

local ttl = redis.call('PTTL', key)
if ttl < 0 then
    redis.call('PEXPIRE', key, window_ms)
    ttl = window_ms
end

if count > limit then
    return { 0, 0, ttl }
end

return { 1, limit - count, ttl }
