#!/usr/bin/env python3
"""Unit checks for is_references_page (SkillGLoW + Benchmark Radar end-of-page refs)."""
from __future__ import annotations

from translate_worker import is_references_page

# Trimmed fixtures mirroring EN PDF pages 8–10 layout (author-year bibliography).
PAGE8_REFS = """\
                        References                                Evaluating LLMs as Agents. In International Conference on
Cao, Z.; Deng, J.; Yu, L.; Zhou, W.; Liu, Z.; Ding, B.; and       Learning Representations (ICLR).
Zhao, H. 2026. Remember Me, Refine Me: A Dynamic Pro-             Madaan, A.; Tandon, N.; Gupta, P.; Hallinan, S.; Gao, L.;
cedural Memory Framework for Experience-Driven Agent              Wiegreffe, S.; Alon, U.; Dziri, N.; Prabhumoye, S.; Yang,
Evolution. In Findings of the Association for Computational       Y.; Gupta, S.; Majumder, B. P.; Hermann, K.; Welleck, S.;
Linguistics: ACL 2026, 16803–16822.                               Yazdanbakhsh, A.; and Clark, P. 2023. Self-Refine: Itera-
Du, J.; Wu, J.; Chen, Y.; Hu, Y.; Li, B.; and Zhou, J. T. 2025.   tive Refinement with Self-Feedback. In Advances in Neu-
Rethinking Agent Design: From Top-Down Workflows to               ral Information Processing Systems (NeurIPS), volume 36,
Bottom-Up Skill Evolution. arXiv:2505.17673.
"""

PAGE9_CONT = """\
T. 2023. Toolformer: Language Models Can Teach Them-            Yuksekgonul, M.; Bianchi, F.; Boen, J.; Liu, S.; Lu, P.;
selves to Use Tools. In Advances in Neural Information          Huang, Z.; Guestrin, C.; and Zou, J. 2025. Optimizing Gen-
Processing Systems, volume 36, 68539–68551.                     erative AI by Backpropagating Language Model Feedback.
Shinn, N.; Cassano, F.; Gopinath, A.; Narasimhan, K.; and       Nature, 639(8055): 609–616.
Yao, S. 2023. Reflexion: Language Agents with Verbal Re-        Zhang, Y.; Li, M.; Long, D.; Zhang, X.; Lin, H.; Yang, B.;
inforcement Learning. In Advances in Neural Information         Xie, P.; Yang, A.; Liu, D.; Lin, J.; Huang, F.; and Zhou, J.
Processing Systems (NeurIPS), volume 36, 8634–8652.             2025. Qwen3 Embedding: Advancing Text Embedding and
Shridhar, M.; Yuan, X.; Côté, M.-A.; Bisk, Y.; Trischler, A.;   Reranking Through Foundation Models. arXiv:2506.05176.
and Hausknecht, M. 2021. ALFWorld: Aligning Text and            Zhang, Z.; Shi, K.; Huang, S.; Nie, A.; Zeng, Y.; Zhao,
Embodied Environments for Interactive Learning. In Inter-       Y.; Fang, Z.; Su, Q.; Qiu, H.; Yang, W.; Ren, Q.; Zou, S.;
national Conference on Learning Representations (ICLR).
"""

PAGE10_APPENDIX = """\
This document supplies the material the paper defers to.                     Channel                       Content
Section letters (§§A–O), tables (Tables A1–A28) and figures
(Figures A1–A3) are local to this document; a table, figure,            S Signature, compact the 2–5-word abstract operation
equation or §-number without a letter points into the main
paper. Reviewers are not obliged to consult this document,
and the paper is self-contained without it. The method is
SkillGLoW in the title and GLoW for short throughout.

                 A     Metric Definitions
The hard metric is the all-or-nothing flag the benchmark
ships with. The soft metric is the benchmark’s own contin-
uous partial credit; ALFWorld has no official partial credit,
so its soft score is derived by our verifier.
"""

# Benchmark Radar 2609.11115: References starts late on the page (after Author
# Contributions); first ~400 chars are still body text.
PAGE13_END_REFS = """\
   Retrieval precision, task suitability, and time saved remain to be evaluated. The worked example
combines local queries with web search and has no controlled baseline. Lexical matching can miss
paraphrases; embedding retrieval was not used in this example.
   Limitations include incomplete coverage of venue proceedings and vendor cards that lack stable
identifiers. Broader coverage requires additional source collection.
Author Contributions
Yuxuan Chen led the project. Junkai Wang contributed review and copyediting.


References
 [1] Mubashara Akhtar, Anka Reuel, Prajna Soni, Sanchit Ahuja, et al. When AI
     benchmarks plateau: A systematic study of benchmark saturation, 2026.
 [2] Anthropic. Introducing Claude Fable 5.1 and Claude Mythos 5.1, 2026.
 [3] Lisa P. Argyle, Ethan C. Busby, Nancy Fulda, et al. Out of one, many:
     Using language models to simulate human samples. Political Analysis, 2023.
"""

PAGE14_NUMBERED_CONT = """\
 [4] Artificial Analysis. Artificial analysis intelligence benchmarking methodology, 2026.
 [5] arXiv.org. arXiv API access, 2026.
 [6] Yuntao Bai, Andy Jones, Kamal Ndousse, et al. Training a helpful and harmless
     assistant with reinforcement learning from human feedback. arXiv:2204.05862, 2022.
 [7] Edward Beeching, Clémentine Fourrier, Nathan Habib, et al. Open LLM Leaderboard.
 [8] Yonatan Bisk, Rowan Zellers, Ronan Le Bras, et al. PIQA. AAAI, 2020.
 [9] Tom B. Brown, Benjamin Mann, Nick Ryder, et al. Language models are few-shot
     learners. NeurIPS, 2020.
[10] Sébastien Bubeck, Varun Chandrasekaran, Ronen Eldan, et al. Sparks of AGI, 2023.
[11] Wei-Lin Chiang, Lianmin Zheng, Ying Sheng, et al. Chatbot Arena. 2024.
[12] Karl Cobbe, Vineet Kosaraju, Mohammad Bavarian, et al. GSM8K. arXiv:2110.14168.
[13] DeepSeek-AI. DeepSeek-V3 technical report. arXiv:2412.19437, 2024.
[14] Dan Hendrycks, Collin Burns, Steven Basart, et al. Measuring massive multitask
     language understanding. ICLR, 2021.
[15] Albert Q. Jiang, Alexandre Sablayrolles, Arthur Mensch, et al. Mixtral of experts.
"""

PAGE18_APPENDIX = """\
                                                                  Appendix


A       Full-Catalog Census
This census retains all 1,283 source records, including those without scores.
"""


def test_skillglow_refs_pages() -> None:
    assert is_references_page(PAGE8_REFS, False) is True
    assert is_references_page(PAGE9_CONT, True) is True
    assert is_references_page(PAGE10_APPENDIX, True) is False
    # Dense author-year continuation is refs even without prev (cite density).
    assert is_references_page(PAGE9_CONT, False) is True


def test_numbered_cite_continuation_still_works() -> None:
    body = "\n".join(f"[{i}] Smith, A. 2020. Title. arXiv:2001.0000{i}." for i in range(1, 12))
    assert is_references_page("References\n" + body, False) is True
    assert is_references_page(body, True) is True
    assert is_references_page(body, False) is True  # density without prev


def test_end_of_page_references_and_continuation() -> None:
    """2609.11115-style: References mid/end of page; numbered continuation pages."""
    assert is_references_page(PAGE13_END_REFS, False) is True
    # Continuation pages without heading or prev still count via cite density.
    assert is_references_page(PAGE14_NUMBERED_CONT, False) is True
    assert is_references_page(PAGE14_NUMBERED_CONT, True) is True
    # Appendix near start ends refs run.
    assert is_references_page(PAGE18_APPENDIX, True) is False


def test_sparse_body_not_refs_without_prev() -> None:
    body = (
        "We discuss related work and cite Smith (2020) once.\n"
        "Further analysis appears in the next section.\n"
        "No bibliography listing is present here.\n"
    )
    assert is_references_page(body, False) is False


if __name__ == "__main__":
    test_skillglow_refs_pages()
    test_numbered_cite_continuation_still_works()
    test_end_of_page_references_and_continuation()
    test_sparse_body_not_refs_without_prev()
    print("test_refs_heuristic: OK")
