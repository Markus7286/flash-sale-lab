-- Acknowledge persisted orders for one SKU.
--
-- KEYS[1] {sku}:orders stream
-- KEYS[2] {sku}:queued string
--
-- ARGV[1] consumer group
-- ARGV[2..] stream_id, qty pairs
--
-- Returns how many entries this call acknowledged. An entry already acknowledged
-- (a redelivery racing its original) is skipped, so queued is decremented once.

local acked = 0
for i = 2, #ARGV, 2 do
  local id = ARGV[i]
  if redis.call('XACK', KEYS[1], ARGV[1], id) == 1 then
    -- Deleting on ack keeps XLEN equal to the backlog.
    redis.call('XDEL', KEYS[1], id)
    redis.call('DECRBY', KEYS[2], ARGV[i + 1])
    acked = acked + 1
  end
end
return acked
