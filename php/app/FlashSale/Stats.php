<?php

namespace App\FlashSale;

final readonly class Stats
{
    public function __construct(
        public int $remaining,
        public int $reservations,
        public int $buyers,
        public int $unitsSold,
    ) {}
}
