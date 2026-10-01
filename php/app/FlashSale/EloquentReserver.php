<?php

namespace App\FlashSale;

use App\Models\Order;
use App\Models\Product;
use Illuminate\Database\UniqueConstraintViolationException;
use Illuminate\Support\Facades\DB;
use RuntimeException;

/**
 * v0b: RawReserver's transaction rewritten in Eloquent, keeping its six round
 * trips and its lock, so the gap between v0 and v0b is the ORM alone — query
 * building, hydration and model events.
 *
 * Eloquent has no builder for UPDATE ... RETURNING, and the idiomatic substitute
 * (decrement, then read back) adds a round trip while the row lock is held; that
 * would measure a different transaction rather than the ORM, so the decrement is
 * raw SQL hydrated through Product::fromQuery().
 */
final class EloquentReserver extends PostgresReserver
{
    public function name(): string
    {
        return 'laravel-eloquent-update';
    }

    public function reserve(string $sku, string $userId, string $requestId, int $qty): Result
    {
        DB::beginTransaction();

        try {
            return $this->reserveInTransaction($sku, $userId, $requestId, $qty);
        } finally {
            if (DB::transactionLevel() > 0) {
                DB::rollBack();
            }
        }
    }

    private function reserveInTransaction(string $sku, string $userId, string $requestId, int $qty): Result
    {
        $duplicate = Order::query()->where('sku', $sku)->where('request_id', $requestId)->exists();
        if ($duplicate) {
            return new Result(Status::Duplicate, $this->stockOf($sku)
                ?? throw new RuntimeException("sku {$sku} vanished under an existing order"));
        }

        $product = Product::fromQuery(<<<'SQL'
            UPDATE products
               SET stock = stock - ?
             WHERE sku = ? AND stock >= ?
            RETURNING stock, per_user_limit
            SQL, [$qty, $sku, $qty])->first();
        if ($product === null) {
            $stock = $this->stockOf($sku);

            return $stock === null
                ? new Result(Status::UnknownSku)
                : new Result(Status::SoldOut, $stock);
        }

        // One statement, as in v0: two Eloquent queries here would add a round trip under the lock.
        $check = Order::query()
            ->selectRaw('EXISTS (SELECT 1 FROM orders WHERE sku = ? AND request_id = ?) AS replayed', [$sku, $requestId])
            ->selectRaw('COALESCE(SUM(qty), 0) AS bought')
            ->where('sku', $sku)
            ->where('user_id', $userId)
            ->firstOrFail();
        if ($check->getAttribute('replayed')) {
            return new Result(Status::Duplicate, $product->stock + $qty);
        }
        if ((int) $check->getAttribute('bought') + $qty > $product->per_user_limit) {
            return new Result(Status::UserLimit, $product->stock + $qty);
        }

        try {
            Order::query()->create([
                'request_id' => $requestId,
                'sku' => $sku,
                'user_id' => $userId,
                'qty' => $qty,
            ]);
        } catch (UniqueConstraintViolationException) {
            return new Result(Status::Duplicate, $product->stock + $qty);
        }

        DB::commit();

        return new Result(Status::Reserved, $product->stock);
    }

    private function stockOf(string $sku): ?int
    {
        return Product::query()->select('stock')->find($sku)?->stock;
    }
}
