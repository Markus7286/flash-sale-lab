<?php

namespace App\FlashSale;

use Illuminate\Database\ConnectionInterface;
use Illuminate\Database\UniqueConstraintViolationException;
use Illuminate\Support\Facades\DB;
use RuntimeException;

/**
 * v0: PGReserver.Reserve statement for statement — the same transaction, the same
 * six round trips, the same SQL — through the query layer but not the ORM, so the
 * gap to v1 is Laravel and PHP-FPM rather than Eloquent.
 */
final class RawReserver extends PostgresReserver
{
    public function name(): string
    {
        return 'laravel-postgres-update';
    }

    public function reserve(string $sku, string $userId, string $requestId, int $qty): Result
    {
        $db = DB::connection();
        $db->beginTransaction();

        try {
            return $this->reserveIn($db, $sku, $userId, $requestId, $qty);
        } finally {
            // A no-op after commit, so every early return undoes the decrement.
            if ($db->transactionLevel() > 0) {
                $db->rollBack();
            }
        }
    }

    private function reserveIn(ConnectionInterface $db, string $sku, string $userId, string $requestId, int $qty): Result
    {
        // Idempotency fast path; the UNIQUE (sku, request_id) index is what actually enforces it.
        $seen = $db->selectOne(
            'SELECT EXISTS (SELECT 1 FROM orders WHERE sku = ? AND request_id = ?) AS duplicate',
            [$sku, $requestId],
        );
        if ($seen->duplicate) {
            return new Result(Status::Duplicate, $this->stockOf($db, $sku)
                ?? throw new RuntimeException("sku {$sku} vanished under an existing order"));
        }

        $product = $db->selectOne(<<<'SQL'
            UPDATE products
               SET stock = stock - ?
             WHERE sku = ? AND stock >= ?
            RETURNING stock, per_user_limit
            SQL, [$qty, $sku, $qty]);
        if ($product === null) {
            $stock = $this->stockOf($db, $sku);

            return $stock === null
                ? new Result(Status::UnknownSku)
                : new Result(Status::SoldOut, $stock);
        }
        $remaining = (int) $product->stock;

        // Checked after the decrement, under the row lock, for the reason postgres.go gives.
        $check = $db->selectOne(<<<'SQL'
            SELECT EXISTS (SELECT 1 FROM orders WHERE sku = ? AND request_id = ?) AS replayed,
                   (SELECT COALESCE(SUM(qty), 0) FROM orders WHERE sku = ? AND user_id = ?) AS bought
            SQL, [$sku, $requestId, $sku, $userId]);
        if ($check->replayed) {
            return new Result(Status::Duplicate, $remaining + $qty);
        }
        if ((int) $check->bought + $qty > (int) $product->per_user_limit) {
            return new Result(Status::UserLimit, $remaining + $qty);
        }

        try {
            $db->insert(
                'INSERT INTO orders (request_id, sku, user_id, qty) VALUES (?, ?, ?, ?)',
                [$requestId, $sku, $userId, $qty],
            );
        } catch (UniqueConstraintViolationException) {
            // A concurrent replay committed since the fast path above.
            return new Result(Status::Duplicate, $remaining + $qty);
        }

        $db->commit();

        return new Result(Status::Reserved, $remaining);
    }

    private function stockOf(ConnectionInterface $db, string $sku): ?int
    {
        $row = $db->selectOne('SELECT stock FROM products WHERE sku = ?', [$sku]);

        return $row === null ? null : (int) $row->stock;
    }
}
