-- Reserve stock for one flash-sale request as a single atomic step.
--
-- KEYS[1] {sku}:stock  string  remaining stock
-- KEYS[2] {sku}:users  hash    user_id -> quantity already bought
-- KEYS[3] {sku}:reqs   set     request_ids already applied (idempotency)
-- KEYS[4] {sku}:sold   string  units sold, so Stats never has to walk KEYS[2]
--
-- ARGV[1] user_id
-- ARGV[2] request_id
-- ARGV[3] quantity requested
-- ARGV[4] per-user purchase limit
--
-- Returns {status, remaining}; status codes mirror flashsale.go, keep them in sync.

local user_id = ARGV[1]
local request_id = ARGV[2]
local qty = tonumber(ARGV[3])
local limit = tonumber(ARGV[4])

-- Idempotency first: a replayed request must never decrement stock twice.
if redis.call('SISMEMBER', KEYS[3], request_id) == 1 then
  return {1, tonumber(redis.call('GET', KEYS[1]) or '0')}
end

local stock = redis.call('GET', KEYS[1])
if stock == false then
  return {4, 0}
end
stock = tonumber(stock)

if stock < qty then
  return {2, stock}
end

local bought = tonumber(redis.call('HGET', KEYS[2], user_id) or '0')
if bought + qty > limit then
  return {3, stock}
end

local remaining = redis.call('DECRBY', KEYS[1], qty)
redis.call('HINCRBY', KEYS[2], user_id, qty)
redis.call('SADD', KEYS[3], request_id)

-- The first sale after a seed creates the bookkeeping keys. They take the stock
-- key's TTL so the per-user limit and idempotency last exactly as long as the
-- stock they guard: expiring earlier would let a buyer through twice.
if redis.call('INCRBY', KEYS[4], qty) == qty then
  local ttl = redis.call('PTTL', KEYS[1])
  if ttl > 0 then
    redis.call('PEXPIRE', KEYS[2], ttl)
    redis.call('PEXPIRE', KEYS[3], ttl)
    redis.call('PEXPIRE', KEYS[4], ttl)
  end
end

return {0, remaining}
