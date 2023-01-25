# What is this?

This is a clone of gh-ost (gh-ost -> spirit) that I hacked up quickly. Originally it didn't do very much, but it's actually **now feature complete**. It just needs more tests, and verification that the final cut-over algorithm is correct.

## How is this different from gh-ost?

- Gh-ost is single threaded and has a lot of throttling code to make sure read-replicas don't get delayed. We don't use read-replicas at Cash, so we can take a few more liberties in trying to run migrations faster. In future I plan to use [github.com/platinummonkey/go-concurrency-limits](https://github.com/platinummonkey/go-concurrency-limits) to limit concurrent requests, but not much more. If a migration is taxing a system too much, we can also kill it and restart it with fewer threads (see next point: it supports resume from checkpoint).
- Spirit supports resume-from-checkpoint (aka it saves its progress every few seconds along the way). This is important for migrations that take several days to complete. This feature is enabled by default and does not require any options.
- The chunk-size is not configurable. Instead you set a chunk-target-in-ms. It will then auto-calculate a chunk-size to meet this target. This is both safer for very wide tables with a lot of indexes and faster for smaller tables.
- It uses simplified libraries for binary log subscribing, and parsing alter statements (canal and the TiDB parser). This saves a significant amount of glue code. The tests are written with testify, rather than a home grown library. The coverage is also pretty good.
- It does not process the binary log events until the copy-rows phase has completed. Instead it builds a map of which rows have changed, and if the last operation was a delete or not. This could also be seen as a limitation, see #2 below.

## Performance

Spirit uses a default of 4 threads and a chunk-target of 500ms. Tests performed on an m1 mac with 10 cores and MySQL 8.0.31:

| Table/Scenario                               | Gh-ost   | spirit  | spirit (checksum disabled) |
| -------------------------------------------- | -------- | ------- | -------------------------- |
| finch.balances (800MB/1M rows), idle load    | 28.720s  | 11.197s | 9.278s                     |
| finch.balances (800MB/1M rows), during bench | 2:50m+   | ~15-18s | ~15-18s                    |


Notes:

* Times during benchmarks vary a lot more. It doesn't seem fair to state a number when there is so much variance, so I just put a range.
* Checksum performance is good while data is in memory. For larger databases performance might be slightly slower, but this feature is generally worth it since it de-risks introducing a new tool substantially. The disabled checksum is more apples-to-apples to gh-ost.
* For the "during bench" test, the migration was started as soon as the load phase of the benchmark was finished. The bench duration was 600s.
* The benchmark might be a worse case for gh-ost, since it prioritizes replication over row-copying and the benchmark never lets up. Spirit takes the opposite approach, and applies replication changes last.

## Risks and Limitations

Writing a new data migration tool is scary, since bugs have real consequences (data loss). Here are the main problems I foresee:

1. The cut-over algorithm is not as battle tested as gh-ost's. I need to understand this better, and make sure it's correct.
2. The binary log subscription strategy is to not advance until the end of the copy-rows. I did this for simplicity and to be more akin to how backup tools commonly work, but it means that binary logs need to be retained for the duration of the migration **for resume from checkpoint to be supported** (might not be a safe assumption). It also might need a lot of memory if the number of distinct rows changed is high (only the PK + if the last modification was a delete or not is retained). I can fix this, but there's then some risk of deadlocks and lock waits because the binlog subscription is now competing with the copy-rows phase. Gh-ost comments that they run these phases one after the other intentionally to avoid this.
3. The optimal configuration (i.e. number of threads, chunk-target-in-ms) is not well understood and doesn't scale based on DB instance size. Since the goal is to be more aggressive than gh-ost, this needs real-world testing to make sure its safe.
4. The chunker expands ranges if the estimated rows is lower than the logical space between min/max key. However, I disabled this for auto_inc keys, since it's likely there are some areas where there aren't gaps in the sequence, and chunks could be very slow. Maybe disabling just for this case is a bad idea, since we hide other cases where dynamic expanding ranges is not good. In the case of auto-inc keys, this could mean very slow migrations where there is a large gap between min/max key. In other cases it could mean stalls as a lot of keys are concentrated in one area.
5. It might not support as many different table types as gh-ost. Currently, primary keys can be int/bigint \[unsigned\] or varbinary. Composite primary keys are supported, but hopefully other types won't be required (we can modify tern to fallback to gh-ost if we need to).
6. Maybe the `KeyAboveHighWatermark` is a useless optimization that complicates the code? If that's the case, removing it would mean non-binary comparable primary keys could be used. I'd love to have some data on this.
