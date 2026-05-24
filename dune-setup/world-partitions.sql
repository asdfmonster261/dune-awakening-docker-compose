-- Seed world_partition rows for all 30 maps.
-- The K8s operator does this from BattleGroup CR spec.database…worldPartitions[];
-- we have no operator, so we do it manually.
--
-- JSON shape matches what the game-server itself writes via save_world_partition:
--   {"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}
-- (snake_case keys, nested 'box' object, 'type' field — confirmed against
-- DuneSandbox/Database/17_proc_world_partition.sql + the SQL schema's
-- determine_partition_label() function reads partition_definition->'box'->>'max_x'.)
--
-- partition_id is BIGSERIAL but we set it EXPLICITLY here: each game-server is
-- launched with -PartitionIndex=N (see docker-compose.yml on-demand services),
-- and load_world_partition(map, identity, dimension, partition_id) joins by
-- partition_id. The N values must match world-template.yaml's id field.
-- After the INSERTs, setval() advances the sequence past 30 so any subsequent
-- inserts (e.g. dynamic partitions) don't collide.

INSERT INTO world_partition (partition_id, map, partition_definition) VALUES
    ( 1, 'Survival_1',                        '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    ( 2, 'Overmap',                           '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    ( 3, 'SH_Arrakeen',                       '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    ( 4, 'SH_HarkoVillage',                   '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    ( 5, 'CB_Story_Hephaestus',               '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    ( 6, 'CB_Story_Ecolab_Carthag',           '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    ( 7, 'CB_Story_WaterFatManor',            '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    ( 8, 'DeepDesert_1',                      '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    ( 9, 'Story_ProcesVerbal',                '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (10, 'DLC_Story_LostHarvest_EcolabA',     '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (11, 'DLC_Story_LostHarvest_EcolabB',     '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (12, 'DLC_Story_LostHarvest_ForgottenLab','{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (13, 'Story_ArtOfKanly',                  '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (14, 'CB_Dungeon_Hephaestus',             '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (15, 'CB_Dungeon_OldCarthag',             '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (16, 'Story_Faction_Outpost_Atre',        '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (17, 'Story_Faction_Outpost_Hark',        '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (18, 'Story_HeighlinerDungeon',           '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (19, 'CB_Ecolab_Bronze_Green_089',        '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (20, 'CB_Ecolab_Bronze_Green_152',        '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (21, 'CB_Ecolab_Bronze_Green_024',        '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (22, 'CB_Ecolab_Bronze_Green_195',        '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (23, 'CB_Ecolab_Bronze_Green_136',        '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (24, 'CB_Overland_M_01',                  '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (25, 'CB_Overland_S_04',                  '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (26, 'CB_Overland_S_06',                  '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (27, 'CB_Story_BanditFortress01',         '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (28, 'CB_Overland_S_07',                  '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (29, 'CB_Overland_S_08',                  '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}'),
    (30, 'CB_Dungeon_ThePit',                 '{"box": {"max_x": 1, "max_y": 1, "min_x": 0, "min_y": 0}, "type": "box2d_array"}')
ON CONFLICT DO NOTHING;

-- Seed world_partition_reset_seed for every partition. Game-servers query
-- (partition_id, dimension, world_reset_seed) from this table on startup;
-- if a partition has no row, only the farm leader (partition 1) self-
-- inserts. Followers stay in S2S_Starting waiting for state that never
-- arrives. The K8s setup gets these rows from the operator; we mirror
-- that here so on-demand maps don't hang on first spawn.
INSERT INTO world_partition_reset_seed (partition_id, world_reset_seed)
SELECT partition_id, 1 FROM world_partition
ON CONFLICT DO NOTHING;

-- Advance the sequence past our explicit IDs so any future BIGSERIAL inserts
-- don't try to reuse 1..30.
SELECT setval(pg_get_serial_sequence('world_partition', 'partition_id'), 30);
