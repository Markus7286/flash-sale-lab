<?php

return [
    // Written to products.per_user_limit at seed time, as the Go api does.
    'per_user_limit' => max(1, (int) env('PER_USER_LIMIT', 1)),
];
