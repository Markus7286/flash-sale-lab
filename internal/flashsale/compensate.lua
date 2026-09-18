-- Undo a reservation whose order can never be persisted, and dead-letter it.
--
-- KEYS[1] {sku}:stock  KEYS[2] {sku}:users  KEYS[3] {sku}:reqs  KEYS[4] {sku}:sold
-- KEYS[5] {sku}:queued KEYS[6] {sku}:orders KEYS[7] {sku}:dead
--
-- ARGV[1] consumer group  ARGV[2] stream_id  ARGV[3] request_id
-- ARGV[4] user_id         ARGV[5] qty        ARGV[6] reason
--
-- Returns 1 if this call compensated, 0 if another consumer already settled the entry.

local group, id, request_id, user_id = ARGV[1], ARGV[2], ARGV[3], ARGV[4]
local qty = tonumber(ARGV[5])

-- XACK is the ownership check: only the first settler of an entry gets a 1.
if redis.call('XACK', KEYS[6], group, id) == 0 then
  return 0
end
redis.call('XDEL', KEYS[6], id)
redis.call('DECRBY', KEYS[5], qty)

-- A reseed or expiry may already have dropped the request_id; then there is
-- nothing left to give back and restoring stock would oversell.
local restored = 0
if redis.call('SREM', KEYS[3], request_id) == 1 then
  redis.call('INCRBY', KEYS[1], qty)
  if redis.call('HINCRBY', KEYS[2], user_id, -qty) <= 0 then
    redis.call('HDEL', KEYS[2], user_id)
  end
  redis.call('DECRBY', KEYS[4], qty)
  restored = 1
end

redis.call('XADD', KEYS[7], '*', 'stream_id', id, 'request_id', request_id,
  'user_id', user_id, 'qty', qty, 'reason', ARGV[6], 'restored', restored)

local ttl = redis.call('PTTL', KEYS[1])
if ttl > 0 then
  redis.call('PEXPIRE', KEYS[7], ttl)
end

return 1
