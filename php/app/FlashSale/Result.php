<?php

namespace App\FlashSale;

final readonly class Result
{
    public function __construct(
        public Status $status,
        public int $remaining = 0,
    ) {}
}
