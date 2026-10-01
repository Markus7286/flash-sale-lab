<?php

namespace App\FlashSale;

/**
 * The PHP twin of flashsale.Reserver: concurrent calls must never drive stock
 * below zero and never apply the same request_id twice.
 */
interface Reserver
{
    public function name(): string;

    public function reserve(string $sku, string $userId, string $requestId, int $qty): Result;

    /** (Re)initialises a SKU and clears its orders. */
    public function seedStock(string $sku, int $stock): void;

    /** Null when the SKU has never been seeded. */
    public function stats(string $sku): ?Stats;
}
