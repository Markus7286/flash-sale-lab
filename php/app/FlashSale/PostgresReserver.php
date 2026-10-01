<?php

namespace App\FlashSale;

use Illuminate\Support\Facades\DB;

/**
 * Seeding and stats, shared by both Laravel backends and identical to PGReserver's,
 * so only reserve() differs between v0 and v0b.
 */
abstract class PostgresReserver implements Reserver
{
    public function __construct(protected readonly int $perUserLimit) {}

    public function seedStock(string $sku, int $stock): void
    {
        DB::transaction(function () use ($sku, $stock) {
            // Take the row lock reserve() takes, so an in-flight reservation commits
            // its order before the DELETE below runs instead of surviving the reset.
            DB::select('SELECT 1 FROM products WHERE sku = ? FOR UPDATE', [$sku]);
            DB::delete('DELETE FROM orders WHERE sku = ?', [$sku]);
            DB::insert(<<<'SQL'
                INSERT INTO products (sku, stock, per_user_limit) VALUES (?, ?, ?)
                ON CONFLICT (sku) DO UPDATE
                   SET stock = EXCLUDED.stock, per_user_limit = EXCLUDED.per_user_limit
                SQL, [$sku, $stock, $this->perUserLimit]);
        }, attempts: 1);
    }

    public function stats(string $sku): ?Stats
    {
        $row = DB::selectOne(<<<'SQL'
            SELECT p.stock, count(o.id) AS reservations, count(DISTINCT o.user_id) AS buyers,
                   COALESCE(sum(o.qty), 0) AS units_sold
              FROM products p
              LEFT JOIN orders o ON o.sku = p.sku
             WHERE p.sku = ?
             GROUP BY p.sku
            SQL, [$sku]);

        if ($row === null) {
            return null;
        }

        return new Stats(
            remaining: (int) $row->stock,
            reservations: (int) $row->reservations,
            buyers: (int) $row->buyers,
            unitsSold: (int) $row->units_sold,
        );
    }
}
