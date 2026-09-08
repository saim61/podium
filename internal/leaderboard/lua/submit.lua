-- Record one score across every period, for the game and for the cross-game total.
--
-- This is one script because it is a read-modify-write across eight keys. A game's entry keeps
-- only a user's BEST score, and the global entry is the SUM of their best per game - so applying
-- a new score means reading the old one, deciding whether it beats it, and moving the global
-- total by exactly the difference. Split across round trips, two concurrent submissions both read
-- the same old value and both add their own delta to the global set, permanently inflating it.
-- Redis runs a script to completion with nothing interleaved, which is what makes the arithmetic
-- hold under load.
--
-- KEYS[1..4]  per-game keys:   all, day, week, month
-- KEYS[5..8]  cross-game keys: all, day, week, month
-- ARGV[1]     member (user id)
-- ARGV[2]     points
-- ARGV[3..6]  ttl seconds per period, 0 meaning never expire

local member = ARGV[1]
local points = tonumber(ARGV[2])
local result = {}

for i = 1, 4 do
    local game_key = KEYS[i]
    local global_key = KEYS[i + 4]
    local ttl = tonumber(ARGV[2 + i])

    local previous = redis.call('ZSCORE', game_key, member)
    local improved = 0

    if previous == false then
        redis.call('ZADD', game_key, points, member)
        redis.call('ZINCRBY', global_key, points, member)
        improved = 1
    else
        previous = tonumber(previous)
        if points > previous then
            redis.call('ZADD', game_key, points, member)
            redis.call('ZINCRBY', global_key, points - previous, member)
            improved = 1
        end
    end

    -- Only when the key has no expiry yet. Setting it on every write would slide the window
    -- forward forever and a daily leaderboard would never actually roll over.
    if ttl > 0 then
        if redis.call('TTL', game_key) < 0 then
            redis.call('EXPIRE', game_key, ttl)
        end
        if redis.call('TTL', global_key) < 0 then
            redis.call('EXPIRE', global_key, ttl)
        end
    end

    local game_score = tonumber(redis.call('ZSCORE', game_key, member)) or 0
    local global_score = tonumber(redis.call('ZSCORE', global_key, member)) or 0

    -- Competition rank: how many members score strictly higher, plus one. Everyone tied on a
    -- score therefore shares a rank, which ZREVRANK would not give.
    result[#result + 1] = improved
    result[#result + 1] = game_score
    result[#result + 1] = redis.call('ZCOUNT', game_key, '(' .. game_score, '+inf') + 1
    result[#result + 1] = global_score
    result[#result + 1] = redis.call('ZCOUNT', global_key, '(' .. global_score, '+inf') + 1
end

return result
